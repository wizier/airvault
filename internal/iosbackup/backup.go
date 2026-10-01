// Package iosbackup is the iOS backup format stored in a snapshot: the plists
// that describe a backup and the keybag behind its password.
package iosbackup

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/objectstore"
)

const (
	infoPlist    = "Info.plist"
	maxPlistSize = 256 << 20 // Info.plist grows with the app census past 64 MiB
)

// Info is a backup's Info.plist: the device and its App Store apps. Finder
// shows it, and a restore reinstalls the apps from it.
type Info struct {
	BuildVersion          string                 `plist:"Build Version,omitempty"`
	DeviceName            string                 `plist:"Device Name,omitempty"`
	DisplayName           string                 `plist:"Display Name,omitempty"`
	GUID                  string                 `plist:"GUID"`
	ICCID                 string                 `plist:"ICCID,omitempty"`
	IMEI                  string                 `plist:"IMEI,omitempty"`
	MEID                  string                 `plist:"MEID,omitempty"`
	PhoneNumber           string                 `plist:"Phone Number,omitempty"`
	ProductType           string                 `plist:"Product Type,omitempty"`
	ProductVersion        string                 `plist:"Product Version,omitempty"`
	SerialNumber          string                 `plist:"Serial Number,omitempty"`
	TargetIdentifier      string                 `plist:"Target Identifier"`
	TargetType            string                 `plist:"Target Type"`
	UniqueIdentifier      string                 `plist:"Unique Identifier"`
	LastBackupDate        time.Time              `plist:"Last Backup Date"`
	ITunesVersion         string                 `plist:"iTunes Version"`
	ITunesSettings        any                    `plist:"iTunes Settings"`
	ITunesFiles           map[string][]byte      `plist:"iTunes Files"`
	IBooksData            []byte                 `plist:"iBooks Data 2,omitempty"`
	Applications          map[string]Application `plist:"Applications"`
	InstalledApplications []string               `plist:"Installed Applications"`
}

// Application is what a restore needs to reinstall an App Store app; the
// store data is opaque.
type Application struct {
	SINF            any    `plist:"ApplicationSINF"`
	Metadata        any    `plist:"iTunesMetadata"`
	PlaceholderIcon []byte `plist:"PlaceholderIcon,omitempty"`
}

// WriteInfo records info as the snapshot's Info.plist.
func WriteInfo(draft *objectstore.Draft, info *Info) error {
	data, err := plist.MarshalIndent(info, plist.XMLFormat, "\t")
	if err != nil {
		return fmt.Errorf("encode Info.plist: %w", err)
	}
	writer, err := draft.Create(infoPlist)
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		writer.Abort()
		return err
	}
	return writer.Commit()
}

// Backup is an iOS backup stored in a snapshot, checked complete.
type Backup struct {
	*objectstore.Snapshot
	Info       Info
	Encrypted  bool
	IOSVersion string
	keybag     []byte
}

// Open reads a snapshot's backup plists and checks the backup is complete
// enough to restore.
func Open(snapshot *objectstore.Snapshot) (*Backup, error) {
	var manifest struct {
		IsEncrypted  bool   `plist:"IsEncrypted"`
		BackupKeyBag []byte `plist:"BackupKeyBag"`
		Lockdown     struct {
			ProductVersion string `plist:"ProductVersion"`
		} `plist:"Lockdown"`
	}
	if err := readPlist(snapshot, "Manifest.plist", &manifest); err != nil {
		return nil, fmt.Errorf("read Manifest.plist: %w", err)
	}
	var status struct {
		SnapshotState string `plist:"SnapshotState"`
	}
	if err := readPlist(snapshot, "Status.plist", &status); err != nil {
		return nil, fmt.Errorf("read Status.plist: %w", err)
	}
	if status.SnapshotState != "finished" {
		return nil, fmt.Errorf("backup Status.plist SnapshotState is %q, want %q", status.SnapshotState, "finished")
	}
	backup := &Backup{Snapshot: snapshot, Encrypted: manifest.IsEncrypted, IOSVersion: manifest.Lockdown.ProductVersion,
		keybag: manifest.BackupKeyBag}
	if err := readPlist(snapshot, infoPlist, &backup.Info); err != nil {
		return nil, fmt.Errorf("read Info.plist: %w", err)
	}
	if backup.Info.TargetIdentifier == "" {
		return nil, errors.New("backup Info.plist has no Target Identifier")
	}
	if size, ok := snapshot.FileSize("Manifest.db"); !ok || size == 0 {
		return nil, errors.New("backup Manifest.db is not a non-empty regular file")
	}
	// Read through: a damaged database fails here, not when the device
	// downloads it from the base mid-backup.
	file, err := snapshot.Open("Manifest.db")
	if err != nil {
		return nil, fmt.Errorf("open Manifest.db: %w", err)
	}
	_, err = io.Copy(io.Discard, file)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("read Manifest.db: %w", err)
	}
	return backup, nil
}

// RestoreApplications is the list a restore hands the device to reinstall
// the App Store apps, nil when there are none.
func (b *Backup) RestoreApplications() ([]byte, error) {
	if len(b.Info.Applications) == 0 {
		return nil, nil
	}
	return plist.MarshalIndent(b.Info.Applications, plist.XMLFormat, "\t")
}

func readPlist(snapshot *objectstore.Snapshot, key string, value any) error {
	file, err := snapshot.Open(key)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPlistSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxPlistSize {
		return fmt.Errorf("plist exceeds %d-byte limit", maxPlistSize)
	}
	_, err = plist.Unmarshal(data, value)
	return err
}

// NewerVersion reports iOS version a strictly newer than b; unknown versions
// never block.
func NewerVersion(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			return av > bv
		}
	}
	return false
}
