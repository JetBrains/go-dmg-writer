package udif

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

// Resource describes one entry that appears under a top-level resource-fork
// "key" in the XML plist (e.g. "blkx", "cSum", "plst", "size").
//
// The plist itself is an emulation of the classic Mac OS resource-fork
// format: each top-level key maps to an array of records, each record has
// an ID, optional name, Attributes word, and a Data payload (base64).
type Resource struct {
	ID         int32
	Name       string
	Attributes uint32 // typically 0x0050 ("hdiutil") for blkx, 0 elsewhere
	Data       []byte // raw bytes; we'll base64-encode for output
}

// ResourceFork is the in-memory equivalent of the XML plist's "resource-fork"
// dictionary: an ordered list of (key, list-of-resources) pairs.
//
// Ordering matters because the plist is hashed indirectly via the master
// checksum and because some readers are sensitive to key order.
type ResourceFork struct {
	Keys []string
	Refs map[string][]Resource
}

// NewResourceFork returns an empty ResourceFork ready to receive Add() calls.
func NewResourceFork() *ResourceFork {
	return &ResourceFork{Refs: map[string][]Resource{}}
}

// Add appends r under the given key, creating the key if it doesn't exist.
func (rf *ResourceFork) Add(key string, r Resource) {
	if _, ok := rf.Refs[key]; !ok {
		rf.Keys = append(rf.Keys, key)
	}
	rf.Refs[key] = append(rf.Refs[key], r)
}

// AttributeHdiutil is the conventional Attributes value that hdiutil sets on
// every blkx resource. fsck/hdiutil verify check for it.
const AttributeHdiutil uint32 = 0x0050

// WriteTo emits the resource fork as the XML plist that lives between the
// data fork and the KOLY trailer in a DMG.
//
// We hand-roll the XML to guarantee byte-for-byte deterministic output;
// encoding/xml shuffles attribute order and re-encodes whitespace in ways
// that would defeat the "build twice, get identical bytes" guarantee.
func (rf *ResourceFork) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	buf.WriteString(`<plist version="1.0">` + "\n")
	buf.WriteString("<dict>\n")
	buf.WriteString("\t<key>resource-fork</key>\n")
	buf.WriteString("\t<dict>\n")
	for _, key := range rf.Keys {
		fmt.Fprintf(&buf, "\t\t<key>%s</key>\n", key)
		buf.WriteString("\t\t<array>\n")
		for _, r := range rf.Refs[key] {
			buf.WriteString("\t\t\t<dict>\n")
			fmt.Fprintf(&buf, "\t\t\t\t<key>Attributes</key>\n\t\t\t\t<string>0x%04X</string>\n", r.Attributes)
			fmt.Fprintf(&buf, "\t\t\t\t<key>CFName</key>\n\t\t\t\t<string>%s</string>\n", xmlEscape(r.Name))
			buf.WriteString("\t\t\t\t<key>Data</key>\n\t\t\t\t<data>\n")
			writeBase64Lines(&buf, r.Data)
			buf.WriteString("\t\t\t\t</data>\n")
			fmt.Fprintf(&buf, "\t\t\t\t<key>ID</key>\n\t\t\t\t<string>%d</string>\n", r.ID)
			fmt.Fprintf(&buf, "\t\t\t\t<key>Name</key>\n\t\t\t\t<string>%s</string>\n", xmlEscape(r.Name))
			buf.WriteString("\t\t\t</dict>\n")
		}
		buf.WriteString("\t\t</array>\n")
	}
	buf.WriteString("\t</dict>\n")
	buf.WriteString("</dict>\n")
	buf.WriteString("</plist>\n")
	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

// writeBase64Lines emits base64 of data wrapped to 64-char lines, indented
// with tabs to match hdiutil's plist style. This makes the produced plist
// visually identical to an hdiutil-created one and easier to diff.
func writeBase64Lines(w io.Writer, data []byte) {
	encoded := base64.StdEncoding.EncodeToString(data)
	const width = 64
	for i := 0; i < len(encoded); i += width {
		end := i + width
		if end > len(encoded) {
			end = len(encoded)
		}
		fmt.Fprintf(w, "\t\t\t\t%s\n", encoded[i:end])
	}
}

func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
