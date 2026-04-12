package main

import (
	"os"
	"path/filepath"
)

func main() {
	var err error
	switch filepath.Base(os.Args[0]) {
	case "balena-extension-manager":
		err = ExecuteManager()
	default:
		err = Execute()
	}
	CloseLogger()
	if err != nil {
		os.Exit(1)
	}
}
