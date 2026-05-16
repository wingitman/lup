package version

// Commit is injected at build time with -ldflags. Development builds keep the
// fallback so update checks can fall back to the source checkout's HEAD.
var Commit = "dev"

// BuildTime is injected at build time for `lup --version`.
var BuildTime = "unknown"
