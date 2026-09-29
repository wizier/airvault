package devicefs

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wizier/airvault/internal/engine"
)

const (
	maxPathBytes      = 4096
	maxComponentBytes = 255
)

// Path is a canonical path relative to the root visible in the UI.
type Path struct{ relative string }

func ParsePath(value string) (Path, error) {
	if value == "" || value == "/" {
		return Path{}, nil
	}
	if len(value) > maxPathBytes {
		return Path{}, fmt.Errorf("invalid device path")
	}
	for component := range strings.SplitSeq(value, "/") {
		if !validComponent(component) {
			return Path{}, fmt.Errorf("invalid device path component")
		}
	}
	return Path{relative: value}, nil
}

func validComponent(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= maxComponentBytes &&
		!strings.ContainsAny(name, "/\x00") && utf8.ValidString(name)
}

func (p Path) String() string { return p.relative }

func (p Path) Name() string {
	if _, name, found := strings.CutLast(p.relative, "/"); found {
		return name
	}
	return p.relative
}

func (p Path) Child(name string) (Path, error) {
	if !validComponent(name) {
		return Path{}, fmt.Errorf("invalid device filename")
	}
	if p.relative == "" {
		return Path{relative: name}, nil
	}
	child := p.relative + "/" + name
	if len(child) > maxPathBytes {
		return Path{}, fmt.Errorf("device path is too long")
	}
	return Path{relative: child}, nil
}

type Root struct {
	source   engine.AFCSource
	bundleID string
	prefix   string
}

func Media() Root { return Root{source: engine.AFCMedia} }

func AppDocuments(bundleID string) (Root, error) {
	if bundleID == "" || len(bundleID) > 512 || strings.ContainsRune(bundleID, '\x00') || !utf8.ValidString(bundleID) {
		return Root{}, fmt.Errorf("invalid bundle id")
	}
	return Root{source: engine.AFCAppDocuments, bundleID: bundleID, prefix: "/Documents"}, nil
}

func (r Root) physical(path Path) (string, error) {
	var physical string
	if path.relative == "" {
		if r.prefix != "" {
			physical = r.prefix
		} else {
			physical = "/"
		}
	} else {
		physical = r.prefix + "/" + path.relative
	}
	if len(physical) > maxPathBytes {
		return "", fmt.Errorf("physical device path is too long")
	}
	return physical, nil
}
