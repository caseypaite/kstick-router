package smscfg

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// keyWords: short, distinct, easy-to-read words for API key names.
var keyWords = strings.Fields(`
amber apple arrow aspen autumn badger bamboo basil beacon berry birch bison bloom breeze brook cactus
canyon cedar cherry cinder clover cobalt comet coral cosmos cotton crane crystal cypress dawn delta
desert dolphin dune eagle ember falcon fern fjord flint forest fossil fox frost galaxy garnet geyser
ginger glacier granite grove harbor hazel heron hollow honey horizon iris island ivory jade jasmine
juniper kestrel lagoon lantern lark laurel lava lemon lily linen lotus lunar maple marble meadow
mesa meteor mint monsoon moss nectar nova oak oasis ocean olive onyx orbit orchid otter owl palm
panda pebble pepper pine planet plum polar poppy prairie quartz rain raven reef ridge river robin
saffron sage salmon sand sapphire shore sierra silver sky slate snow solar sparrow spruce star stone
storm summit sun swan thunder tide tiger topaz trail tulip tundra valley velvet violet walnut wave
willow wind winter wolf zephyr
`)

func pick(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err)
	}
	return int(v.Int64())
}

// RandomKeyName returns three different random words joined by "-",
// avoiding names in taken (case-insensitive).
func RandomKeyName(taken func(string) bool) string {
	for {
		a, b, c := pick(len(keyWords)), pick(len(keyWords)), pick(len(keyWords))
		if a == b || b == c || a == c {
			continue
		}
		n := keyWords[a] + "-" + keyWords[b] + "-" + keyWords[c]
		if taken == nil || !taken(n) {
			return n
		}
	}
}
