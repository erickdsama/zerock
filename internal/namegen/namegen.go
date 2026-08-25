// Package namegen produces the friendly random subdomains used when the caller
// does not ask for a specific one.
package namegen

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var adjectives = []string{
	"amber", "brave", "calm", "clever", "cosmic", "crisp", "dawn", "eager",
	"early", "fair", "fancy", "gentle", "giddy", "glad", "golden", "happy",
	"hazel", "ivory", "jolly", "keen", "lively", "lucky", "mellow", "merry",
	"misty", "noble", "olive", "plucky", "proud", "quiet", "rapid", "royal",
	"rustic", "silent", "silver", "smooth", "snappy", "solar", "spry", "still",
	"sunny", "swift", "tidy", "vivid", "warm", "wise", "witty", "young",
}

var nouns = []string{
	"anchor", "arrow", "badger", "beacon", "birch", "brook", "canyon", "cedar",
	"comet", "coral", "cypress", "delta", "dune", "ember", "falcon", "fern",
	"forge", "grove", "harbor", "heron", "island", "jasper", "kestrel", "lantern",
	"ledge", "lynx", "maple", "meadow", "mesa", "otter", "peak", "pine",
	"quartz", "raven", "reef", "ridge", "river", "sable", "shale", "slate",
	"sparrow", "spruce", "summit", "thicket", "tundra", "vale", "willow", "wren",
}

const hexDigits = "0123456789abcdef"

// New returns a subdomain like "swift-otter-4f2". The 12 bits of suffix make
// collisions unlikely; callers retry on the rare clash.
func New() string {
	var suffix strings.Builder
	for range 3 {
		suffix.WriteByte(hexDigits[pick(len(hexDigits))])
	}
	return fmt.Sprintf("%s-%s-%s", adjectives[pick(len(adjectives))], nouns[pick(len(nouns))], suffix.String())
}

func pick(n int) int {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// crypto/rand does not fail on any platform we support; if it somehow
		// does there is nothing sensible to fall back to.
		panic("namegen: " + err.Error())
	}
	return int(i.Int64())
}

// validLabel matches a DNS label: lowercase alphanumerics and inner hyphens.
var validLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidSubdomain reports whether s is usable as a single DNS label. It does not
// check availability.
func ValidSubdomain(s string) bool {
	return validLabel.MatchString(s) && !strings.Contains(s, "--")
}
