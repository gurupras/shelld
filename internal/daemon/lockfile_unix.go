//go:build unix

package daemon

import "os"

func replaceFile(tmp, path string) error { return os.Rename(tmp, path) }

func removeFile(path string) error { return os.Remove(path) }

func readFileShared(path string) ([]byte, error) { return os.ReadFile(path) }
