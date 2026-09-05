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
		modeFlag = flag.String("mode", "udzo", "output mode: udro | udzo")
		nameFlag = flag.String("name", "disk image", "volume name as it appears in Finder")
		timeFlag = flag.Int64("time", 0, "Unix timestamp baked into the volume (0 = now)")
		gptFlag  = flag.Bool("gpt", false, "frame the volume in a GUID partition table")
	)
	flag.Parse()
	if *srcFlag == "" || *outFlag == "" {
		flag.Usage()
		os.Exit(2)
	}

	var mode dmg.Mode
	switch strings.ToLower(*modeFlag) {
	case "udro":
		mode = dmg.ModeReadOnly
	case "udzo":
		mode = dmg.ModeReadOnlyCompressed
	default:
		log.Fatalf("unknown -mode %q (want udro or udzo)", *modeFlag)
	}

	var when time.Time
	if *timeFlag != 0 {
		when = time.Unix(*timeFlag, 0).UTC()
	}

	d := &dmg.DMG{
		VolumeName:   *nameFlag,
		Time:         when,
		PartitionMap: *gptFlag,
	}
	if err := d.Create(*srcFlag, *outFlag, mode); err != nil {
		log.Fatalf("dmg create: %v", err)
	}
	// The image is already written at this point, so a failure to stat it
	// is not worth exiting over; report the path without the size.
	if st, err := os.Stat(*outFlag); err == nil {
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *outFlag, st.Size())
	} else {
		fmt.Fprintf(os.Stderr, "wrote %s\n", *outFlag)
	}
}
