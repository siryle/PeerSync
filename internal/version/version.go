// Package version is the client version shown in the window and by the CLI.
package version

// Current is the PeerSync client version.
const Current = "0.3.0"

// Build is the macOS bundle version. It increases by one on each release.
const Build = "2"

// Window is "1" when this binary should open a window if it is started
// with no arguments. The Windows GUI build sets it with -X.
var Window string
