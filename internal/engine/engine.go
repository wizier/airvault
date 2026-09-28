// Package engine talks to iOS devices over usbmuxd or netmuxd: discovery,
// pairing, device services, file access and backup transfers.
package engine

import (
	"fmt"
	"path/filepath"

	"github.com/wizier/airvault/internal/ios"
)

type DeviceID string

type Config struct {
	PairingRoot string
	MuxAddress  string
}

type Engine struct {
	mux   ios.Mux
	pairs *pairStore
	afc   *afcPool
}

func New(config Config) (*Engine, error) {
	mux, err := ios.ParseMux(config.MuxAddress)
	if err != nil {
		return nil, err
	}
	pairingRoot, err := filepath.Abs(config.PairingRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve engine pairing root: %w", err)
	}
	return &Engine{mux: mux, pairs: &pairStore{root: pairingRoot}, afc: newAFCPool()}, nil
}

func (e *Engine) Close() { e.afc.close() }
