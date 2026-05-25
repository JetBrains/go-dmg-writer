// mkdmg is a tiny demo CLI that wraps [dmg.DMG.Create]. Build with:
//
//	go install ./examples/mkdmg
//
// Usage:
//
//	mkdmg -src ./build/MyApp -out MyApp.dmg -mode udzo -name MyApp
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jetbrains/go-dmg-writer"
)

func main() {
	var (
		srcFlag  = flag.String("src", "", "source folder (required)")
		outFlag  = flag.String("out", "", "output .dmg path (required)")
		modeFlag = flag.String("mode", "udzo", "output mode: udrw | udro | udzo")
		nameFlag = flag.String("name", "disk image", "volume name as it appears in Finder")
		timeFlag = flag.Int64("time", 0, "Unix timestamp baked into the volume (0 = now)")
	)
	flag.Parse()
	if *srcFlag == "" || *outFlag == "" {
		flag.Usage()
		os.Exit(2)
	}

	var mode dmg.Mode
	switch strings.ToLower(*modeFlag) {
	case "udrw":
		mode = dmg.ModeReadWrite
	case "udro":
		mode = dmg.ModeReadOnly
	case "udzo":
		mode = dmg.ModeReadOnlyCompressed
	default:
		log.Fatalf("unknown -mode %q (want udrw, udro, or udzo)", *modeFlag)
	}

	var when time.Time
	if *timeFlag != 0 {
		when = time.Unix(*timeFlag, 0).UTC()
	}

	d := &dmg.DMG{
		VolumeName: *nameFlag,
		Time:       when,
	}
	if err := d.Create(*srcFlag, *outFlag, mode); err != nil {
		log.Fatalf("dmg create: %v", err)
	}
	st, _ := os.Stat(*outFlag)
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *outFlag, st.Size())
}
