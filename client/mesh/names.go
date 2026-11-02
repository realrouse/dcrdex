// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mesh

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Random public display names ("calm-otter-4821"). They are the default for
// the mesh client and the embedded mesh node, so nothing that identifies the
// computer or its user (host name, user name) is announced to the mesh.

var nameAdjectives = []string{
	"amber", "bold", "brave", "bright", "brisk", "calm", "clever", "cool", "crisp", "daring",
	"eager", "fair", "fast", "fierce", "gentle", "glad", "golden", "grand", "happy", "hardy",
	"keen", "kind", "lively", "lucky", "mellow", "merry", "mighty", "misty", "noble", "patient",
	"plucky", "proud", "quick", "quiet", "rapid", "rustic", "sharp", "shy", "silent", "silver",
	"sly", "smooth", "solid", "steady", "stout", "sunny", "swift", "tidy", "true", "vivid",
	"warm", "wild", "wise", "witty", "young", "zesty",
}

var nameAnimals = []string{
	"antelope", "badger", "beaver", "bison", "bobcat", "buffalo", "camel", "caribou", "cheetah", "condor",
	"coyote", "crane", "dingo", "dolphin", "eagle", "elk", "falcon", "ferret", "finch", "fox",
	"gazelle", "gecko", "heron", "hawk", "ibex", "jackal", "jaguar", "kestrel", "koala", "lemur",
	"lynx", "marmot", "marten", "moose", "narwhal", "ocelot", "orca", "osprey", "otter", "owl",
	"panda", "panther", "pelican", "puffin", "puma", "raven", "seal", "stoat", "swan", "tapir",
	"tiger", "walrus", "weasel", "wolf", "wombat", "yak", "zebra",
}

func randUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return binary.BigEndian.Uint32(b[:])
}

// RandomName returns a random adjective-animal-NNNN display name.
func RandomName() string {
	a := nameAdjectives[randUint32()%uint32(len(nameAdjectives))]
	n := nameAnimals[randUint32()%uint32(len(nameAnimals))]
	return fmt.Sprintf("%s-%s-%04d", a, n, randUint32()%10000)
}

// LoadOrCreateName returns the display name saved at path, or creates,
// saves and returns a new random one. The name stays the same across
// restarts, so peers recognize us, but it says nothing about the computer.
func LoadOrCreateName(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if s := SanitizeName(strings.TrimSpace(string(b))); s != "" {
			return s, nil
		}
	}
	name := RandomName()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return name, err
	}
	return name, os.WriteFile(path, []byte(name+"\n"), 0o600)
}
