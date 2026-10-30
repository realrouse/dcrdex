// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package lexi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"decred.org/dcrdex/dex"
	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/pb"
	"github.com/dgraph-io/badger/v4/table"
	"github.com/dgraph-io/badger/v4/y"
	"google.golang.org/protobuf/proto"
)

// CorruptTable checks the footer of every badger v4 table (*.sst) file in
// dir that is referenced by the DB's MANIFEST (or every table file, if the
// MANIFEST can't be read) and returns the path of the first one that badger would be unable to
// open, along with a description of the problem. An empty path means no
// corrupt table was found. A missing dir is not an error.
//
// A badger table ends with [index][index len (4)][checksum][checksum len (4)].
// Badger reads this footer in a background goroutine while opening the DB and
// panics (crashing the process) on tables whose footer is truncated or
// zero-filled, as happens when the OS loses unsynced writes after a power loss
// or crash, or when the disk fills up. This check mirrors badger's footer
// parsing and index checksum verification without panicking. Unreferenced
// table files, e.g. a partially written table from an interrupted compaction,
// are skipped because badger deletes them on open without reading them.
func CorruptTable(dir string) (string, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		return "", "", err
	}
	referenced := manifestTableIDs(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sst") {
			continue
		}
		if referenced != nil {
			if id, ok := table.ParseFileID(e.Name()); ok {
				if _, found := referenced[id]; !found {
					continue
				}
			}
		}
		path := filepath.Join(dir, e.Name())
		problem, err := checkTableFooter(path)
		if err != nil {
			return "", "", err
		}
		if problem != "" {
			return path, problem, nil
		}
	}
	return "", "", nil
}

// manifestTableIDs returns the IDs of the tables referenced by the MANIFEST in
// dir, or nil if the MANIFEST is missing or unreadable.
func manifestTableIDs(dir string) map[uint64]struct{} {
	f, err := os.Open(filepath.Join(dir, badger.ManifestFilename))
	if err != nil {
		return nil
	}
	defer f.Close()
	mf, _, err := badger.ReplayManifestFile(f, 0, badger.DefaultOptions(dir))
	if err != nil {
		return nil
	}
	ids := make(map[uint64]struct{}, len(mf.Tables))
	for id := range mf.Tables {
		ids[id] = struct{}{}
	}
	return ids
}

// checkTableFooter returns a non-empty description of the problem if the
// table file at path does not have a valid footer and index checksum. A
// non-nil error is only returned if the file could not be read.
func checkTableFooter(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()
	readAt := func(off, n int64) ([]byte, error) {
		b := make([]byte, n)
		if _, err := f.ReadAt(b, off); err != nil && !(errors.Is(err, io.EOF) && off+n == size) {
			return nil, err
		}
		return b, nil
	}

	const u32 = 4
	if size < 2*u32 {
		return fmt.Sprintf("table file is %d bytes, too small to hold a footer", size), nil
	}
	pos := size - u32
	b, err := readAt(pos, u32)
	if err != nil {
		return "", err
	}
	checksumLen := int64(binary.BigEndian.Uint32(b))
	if checksumLen > pos-u32 {
		return fmt.Sprintf("checksum length %d exceeds table size %d", checksumLen, size), nil
	}
	pos -= checksumLen
	chkBytes, err := readAt(pos, checksumLen)
	if err != nil {
		return "", err
	}
	chk := new(pb.Checksum)
	if err := proto.Unmarshal(chkBytes, chk); err != nil {
		return fmt.Sprintf("undecodable index checksum: %v", err), nil
	}
	pos -= u32
	b, err = readAt(pos, u32)
	if err != nil {
		return "", err
	}
	indexLen := int64(binary.BigEndian.Uint32(b))
	// The index is a flatbuffer, which is at least a 4-byte root offset.
	// A zero-filled file has a zero index length, which is what makes badger
	// panic with "index out of range [3] with length 0".
	if indexLen < u32 {
		return fmt.Sprintf("index length %d is too small (zero-filled table?)", indexLen), nil
	}
	if indexLen > pos {
		return fmt.Sprintf("index length %d exceeds table size %d", indexLen, size), nil
	}
	index, err := readAt(pos-indexLen, indexLen)
	if err != nil {
		return "", err
	}
	if err := y.VerifyChecksum(index, chk); err != nil {
		return fmt.Sprintf("index checksum mismatch: %v", err), nil
	}
	if rootOff := int64(binary.LittleEndian.Uint32(index)); rootOff+u32 > indexLen {
		return fmt.Sprintf("index root offset %d out of range for index length %d", rootOff, indexLen), nil
	}
	return "", nil
}

// MoveAsideIfCorrupt checks the badger DB directory dir with CorruptTable. If
// a corrupt table is found, the whole directory is renamed to
// "<dir>.corrupt-<UTC yyyymmddhhmmss>" so that a fresh DB can be created in
// its place, and the new path is returned. Nothing is deleted. This must only
// be used for databases whose contents can be regenerated, e.g. caches.
func MoveAsideIfCorrupt(dir string, log dex.Logger) (string, error) {
	badTable, problem, err := CorruptTable(dir)
	if err != nil || badTable == "" {
		return "", err
	}
	dir = strings.TrimRight(dir, `/\`)
	newPath := fmt.Sprintf("%s.corrupt-%s", dir, time.Now().UTC().Format("20060102150405"))
	if err := os.Rename(dir, newPath); err != nil {
		return "", fmt.Errorf("database %s has a corrupt table %s (%s) and could not be moved aside: %w",
			dir, badTable, problem, err)
	}
	log.Warnf("Database %s has a corrupt table file %s (%s), probably from an unclean shutdown or full disk. "+
		"Moved it aside to %s and starting a fresh one.", dir, filepath.Base(badTable), problem, newPath)
	return newPath, nil
}
