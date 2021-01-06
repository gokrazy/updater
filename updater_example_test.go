package updater_test

import (
	"io"
	"log"
	"net/http"

	"github.com/gokrazy/updater"
)

// obtained from elsewhere, not relevant for this example
var rootReader, bootReader, mbrReader io.Reader

func Example() {
	const baseURL = "http://gokrazy:example@gokrazy/"
	target, err := updater.NewTarget(baseURL, http.DefaultClient)
	if err != nil {
		log.Fatalf("checking target partuuid support: %v", err)
	}

	// Start with the root file system because writing to the non-active
	// partition cannot break the currently running system.
	if err := target.StreamTo("root", rootReader); err != nil {
		log.Fatalf("updating root file system: %v", err)
	}

	if err := target.StreamTo("boot", bootReader); err != nil {
		log.Fatalf("updating boot file system: %v", err)
	}

	// Only relevant when running on PCs (e.g. router7), the Raspberry Pi does
	// not use an MBR.
	if err := target.StreamTo("mbr", mbrReader); err != nil {
		log.Fatalf("updating MBR: %v", err)
	}

	if err := target.Switch(); err != nil {
		log.Fatalf("switching to non-active partition: %v", err)
	}

	if err := target.Reboot(); err != nil {
		log.Fatalf("reboot: %v", err)
	}
}
