package iosbackup

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"
)

const (
	notesDomain   = "AppDomainGroup-group.com.apple.notes"
	notesDatabase = "NoteStore.sqlite"
)

type Note struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Folder   string    `json:"folder,omitempty"`
	Modified time.Time `json:"modified,omitzero"`
	// Text is what follows the title, U+FFFC where each of the attachments
	// sits, in their order.
	Text        string       `json:"text,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Locked      bool         `json:"locked,omitempty"` // password-protected: its text stays encrypted
}

// resolvedAttachment is what an attachment's identifier stands for in the
// text: the text of an inline one, such as a hashtag or a mention, or a file.
type resolvedAttachment struct {
	inline string
	file   Attachment
}

// Notes reads the notes, the latest edited first.
func (c *Contents) Notes(ctx context.Context) ([]Note, error) {
	db, err := c.domainDatabase(ctx, notesDomain, notesDatabase)
	if err != nil {
		return nil, err
	}
	// Attachments are a convenience: the text stands without them.
	attachments, err := c.noteAttachments(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "note attachments unreadable", "error", err)
	}
	notes := []Note{}
	for rows, err := range c.rows(ctx, db, `SELECT n.Z_PK, COALESCE(n.ZTITLE1, ''), COALESCE(f.ZTITLE2, ''),
			n.ZMODIFICATIONDATE1, COALESCE(n.ZISPASSWORDPROTECTED, 0) != 0, d.ZDATA
		FROM ZICCLOUDSYNCINGOBJECT n JOIN ZICNOTEDATA d ON d.ZNOTE = n.Z_PK
		LEFT JOIN ZICCLOUDSYNCINGOBJECT f ON f.Z_PK = n.ZFOLDER
		WHERE COALESCE(n.ZMARKEDFORDELETION, 0) = 0 ORDER BY n.ZMODIFICATIONDATE1 DESC`) {
		var note Note
		var modified sql.NullFloat64
		var data []byte
		if err == nil {
			err = rows.Scan(&note.ID, &note.Title, &note.Folder, &modified, &note.Locked, &data)
		}
		if err != nil {
			return nil, fmt.Errorf("read the notes: %w", err)
		}
		note.Modified = coreDataTime(modified)
		if !note.Locked {
			note.Text, note.Attachments = noteBody(data, attachments)
			// Notes titles a note by its first line.
			if rest, ok := strings.CutPrefix(note.Text, note.Title); ok && (rest == "" || rest[0] == '\n') {
				note.Text = strings.TrimSpace(rest)
			}
		}
		// A draft Notes never shows: no title, text or attachment.
		if note.Title != "" || note.Text != "" || note.Locked {
			notes = append(notes, note)
		}
	}
	return notes, nil
}

// noteBody reads a note's text and attachments. ZDATA is a gzipped protobuf:
// the note (field 3) of the document (field 2) holds the text (2) and its
// attribute runs (5); each attachment holds a U+FFFC whose run names it (12).
func noteBody(data []byte, attachments map[string]resolvedAttachment) (string, []Attachment) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", nil
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return "", nil
	}
	note := protoField(protoField(raw, 2), 3)
	var ids []string
	for _, run := range protoFields(note, 5) {
		if info := protoField(run, 12); info != nil {
			ids = append(ids, string(protoField(info, 1)))
		}
	}
	var text strings.Builder
	var files []Attachment
	pieces := strings.Split(string(protoField(note, 2)), "\uFFFC")
	for i, piece := range pieces {
		text.WriteString(piece)
		if i == len(pieces)-1 || i >= len(ids) {
			continue
		}
		attachment, ok := attachments[ids[i]]
		switch {
		case !ok:
		case attachment.inline != "":
			text.WriteString(attachment.inline)
		default:
			text.WriteString("\uFFFC")
			files = append(files, attachment.file)
		}
	}
	return strings.TrimSpace(text.String()), files
}

// noteAttachments reads the attachments by their identifier, with the files
// they show.
func (c *Contents) noteAttachments(ctx context.Context, db *sql.DB) (map[string]resolvedAttachment, error) {
	stored, err := c.storedPaths(ctx, notesDomain, "")
	if err != nil {
		return nil, err
	}
	files := newNoteFiles(stored)
	attachments := map[string]resolvedAttachment{}
	for rows, err := range c.rows(ctx, db, `SELECT COALESCE(a.ZIDENTIFIER, ''), a.ZTYPEUTI, COALESCE(a.ZALTTEXT, ''),
			COALESCE(a.ZURLSTRING, ''), COALESCE(m.ZIDENTIFIER, ''), COALESCE(m.ZFILENAME, '')
		FROM ZICCLOUDSYNCINGOBJECT a LEFT JOIN ZICCLOUDSYNCINGOBJECT m ON m.Z_PK = a.ZMEDIA WHERE a.ZTYPEUTI IS NOT NULL`) {
		var id, uti, alt, url, media, filename string
		if err == nil {
			err = rows.Scan(&id, &uti, &alt, &url, &media, &filename)
		}
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(uti, "com.apple.notes.inlinetextattachment") {
			attachments[id] = resolvedAttachment{inline: alt}
			continue
		}
		file := files.find(id, media, filename)
		name := cmp.Or(filename, url)
		if name == "" && file != "" {
			name = path.Base(file)
		}
		attachments[id] = resolvedAttachment{file: Attachment{Name: cmp.Or(name, attachmentLabel(uti)), Path: file,
			Missing: file == "" && media != ""}}
	}
	// A scanned document holds no file: its pages are attachments of their own,
	// created in the order scanned. Without them it keeps its name.
	pages, err := c.notePages(ctx, db, files)
	if err != nil {
		slog.WarnContext(ctx, "scanned pages unreadable", "error", err)
	}
	for id, list := range pages {
		if attachment, ok := attachments[id]; ok {
			attachment.file.Pages = list
			attachments[id] = attachment
		}
	}
	return attachments, nil
}

// noteFiles finds attachments' files among the notes' ones: by the folder an
// attachment's or its media's identifier names, else by the file's name.
type noteFiles struct{ byFolder, byName map[string]string }

func newNoteFiles(stored map[string]bool) noteFiles {
	files := noteFiles{byFolder: map[string]string{}, byName: map[string]string{}}
	for file := range stored {
		parts := strings.Split(file, "/")
		for i, part := range parts[:len(parts)-1] {
			if part == "Media" || part == "FallbackImages" {
				id := strings.TrimSuffix(parts[i+1], path.Ext(parts[i+1]))
				files.byFolder[id] = max(files.byFolder[id], file) // the latest of a media's generations
				break
			}
		}
		files.byName[path.Base(file)] = file
	}
	return files
}

// find prefers an attachment's own image, such as a processed scan or a
// drawing, to its media.
func (f noteFiles) find(attachment, media, filename string) string {
	return cmp.Or(f.byFolder[attachment], f.byFolder[media], f.byName[filename])
}

// notePages lists the pages of each scanned document, by its identifier; a
// page shows its processed image when the backup has one.
func (c *Contents) notePages(ctx context.Context, db *sql.DB, files noteFiles) (map[string][]Attachment, error) {
	pages := map[string][]Attachment{}
	for rows, err := range c.rows(ctx, db, `SELECT p.ZIDENTIFIER, COALESCE(a.ZIDENTIFIER, ''), COALESCE(m.ZIDENTIFIER, ''),
			COALESCE(m.ZFILENAME, '')
		FROM ZICCLOUDSYNCINGOBJECT a JOIN ZICCLOUDSYNCINGOBJECT p ON p.Z_PK = a.ZPARENTATTACHMENT
		LEFT JOIN ZICCLOUDSYNCINGOBJECT m ON m.Z_PK = a.ZMEDIA
		WHERE COALESCE(a.ZMARKEDFORDELETION, 0) = 0 ORDER BY a.ZPARENTATTACHMENT, a.Z_PK`) {
		var document, id, media, filename string
		if err == nil {
			err = rows.Scan(&document, &id, &media, &filename)
		}
		if err != nil {
			return nil, err
		}
		file, name := files.find(id, media, filename), filename
		if name == "" && file != "" {
			name = path.Base(file)
		}
		pages[document] = append(pages[document], Attachment{Name: cmp.Or(name, "Page"), Path: file, Missing: file == ""})
	}
	return pages, nil
}

// attachmentLabel names an attachment with nothing to open.
func attachmentLabel(uti string) string {
	switch {
	case uti == "com.apple.notes.table":
		return "Table"
	case strings.HasPrefix(uti, "com.apple.drawing"), uti == "com.apple.paper":
		return "Drawing"
	case uti == "com.apple.notes.gallery":
		return "Scanned document"
	}
	return uti
}
