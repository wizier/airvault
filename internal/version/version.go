package version

// AppVersion is stamped via -ldflags -X at release build (see Makefile);
// "dev" marks an unstamped build.
var AppVersion = "dev"
