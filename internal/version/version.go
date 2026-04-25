// Package version exposes build metadata for burndown.
package version

// Current is the semantic version of the burndown driver.
//
// Bumped on each meaningful release. The launchd plist and the morning digest
// both surface this so an out-of-date scheduled job is easy to spot.
const Current = "0.1.0-pre"

// String returns the version with a leading "v".
func String() string {
	return "v" + Current
}
