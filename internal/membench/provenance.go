package membench

// Code-state provenance for the versioned report (E01 review r3). A
// report's Manifest.Commit is a caller-supplied build label (the CLI
// defaults it to the binary's version string, "dev" in a dev build): it
// cannot identify the source that produced the results. Report
// .SourceRevision does: it is read from the running binary's OWN
// embedded VCS build info, so the identity is the code the binary was
// compiled from - never the git repository the binary happened to be
// invoked in (an invocation repo is unrelated context: a candidate
// binary run from another project's checkout must not record that
// checkout's HEAD as its source). When the binary carries no VCS stamp
// the provenance is explicitly "unknown" - never a guessed, borrowed or
// build-stamp value presented as a reproducibility identity.

import (
	"encoding/hex"
	"runtime/debug"
)

// SourceRevisionUnknown is recorded when the running binary carries no
// embedded VCS revision: built with -buildvcs=false, built from outside
// a VCS checkout (e.g. a module-proxy download), or - as with every
// `go test` binary - built by tooling that deliberately omits the stamp.
const SourceRevisionUnknown = "unknown"

// BuildSourceRevision returns the code-state provenance of the running
// binary from its embedded build info (runtime/debug.ReadBuildInfo,
// stamped by the go tool at build time):
//
//   - "<40-hex>": the VCS revision the binary's main module was built
//     from, when the build tree was clean;
//   - "<40-hex>+modified": the same revision with uncommitted changes
//     present at build time (vcs.modified=true). The flag is the build
//     tool's own word on tree state: when the tool cannot verify the
//     state it stamps nothing at all, which resolves to "unknown"
//     below - an exact dirty-content claim is never fabricated;
//   - "unknown": no build info or no VCS stamp embedded.
//
// The value is a function of the binary alone, so it is constant across
// invocation working directories and cannot be poisoned by an unrelated
// repository the binary is run from.
func BuildSourceRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return SourceRevisionUnknown
	}
	var rev, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return SourceRevisionUnknown
	}
	if b, err := hex.DecodeString(rev); err != nil || len(b) < 20 {
		// Not a commit hash: the stamp cannot identify the source.
		return SourceRevisionUnknown
	}
	if modified == "true" {
		return rev + "+modified"
	}
	return rev
}
