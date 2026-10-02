//go:build !windows && !linux

package shelld

// kernelIgnoredMask cannot be answered here, so dontPassOnIgnoredSignals falls back to
// signal.Ignored with the two gaps that function has. What that costs is stated there.
//
// In practice this is darwin: the pty package has files for linux, darwin and Windows only, so
// nothing else in the module builds.
//
// Nothing portable is being passed up. /proc/self/status is Linux's; darwin answers the same
// question through sigaction(2), which cannot be asked from Go without either cgo or a
// hand-rolled struct layout, and there is no darwin host here to check one against. Guessing at
// a layout on a platform nobody has run it on is a worse trade than a documented gap that the
// tests measure the shape of -- see TestIgnoredNowFallsBackToTheSignalPackage, which exercises
// this branch, and TestTheCheckAndTheNotificationAreOneStep, which runs on it deliberately.
func kernelIgnoredMask() (mask uint64, known bool) { return 0, false }
