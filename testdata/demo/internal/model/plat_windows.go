//go:build windows

package model

// PathSep is the platform path separator.
const PathSep = "\\"

// Platform names the current platform.
func Platform() string { return "windows" }
