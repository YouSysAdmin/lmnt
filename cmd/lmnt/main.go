// Command lmnt mounts Linux-native disks on macOS through a small Linux
// guest and shares them back over SMB, FTP or SFTP.
package main

import (
	"os"

	"github.com/yousysadmin/lmnt/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
