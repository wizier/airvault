// Package version holds build identity, stamped at release build time via
// -ldflags "-X .../internal/version.AppVersion=$(git describe)" (see Makefile).
// The SPA gets the same value at build time through VITE_APP_VERSION.
package version

// AppVersion is the release tag; "dev" marks an unstamped build.
var AppVersion = "dev"
