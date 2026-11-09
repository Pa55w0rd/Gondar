package hostd

import "os"

// The privileged hostd command does not run on Windows. Keep portable metadata
// tests possible; Linux installations enforce the actual root/daemon ownership.
func installerOwnsFile(os.FileInfo) bool { return true }
