package plumbing

import "os"

// os_OpenFileAppend is a test helper to append raw bytes to a file.
func os_OpenFileAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}
