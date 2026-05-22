// Package petname generates short, memorable identifiers in the form
// adjective-animal-hex4 (e.g. "wandering-fox-3a4b"). Used when a worktree
// is created without a user-chosen name.
package petname

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

var adjectives = []string{
	"admiring", "ancient", "beautiful", "bold", "brave", "calm", "clever",
	"curious", "daring", "dreamy", "eager", "elegant", "epic", "festive",
	"focused", "frosty", "gallant", "gentle", "graceful", "happy", "hardcore",
	"jolly", "keen", "kind", "loving", "lucid", "mystic", "mystifying",
	"naughty", "nostalgic", "peaceful", "playful", "quizzical", "radiant",
	"romantic", "sharp", "silly", "sleepy", "stoic", "tender", "thirsty",
	"trusting", "vibrant", "vigilant", "wandering", "wise", "wonderful", "zealous",
}

var animals = []string{
	"badger", "bear", "beaver", "buffalo", "camel", "cheetah", "cougar",
	"crow", "deer", "dolphin", "eagle", "elephant", "falcon", "ferret",
	"fox", "giraffe", "goose", "hare", "hawk", "heron", "iguana", "jackal",
	"kangaroo", "koala", "lemur", "leopard", "lion", "lynx", "mongoose",
	"narwhal", "ocelot", "otter", "owl", "panda", "panther", "platypus",
	"puma", "raccoon", "raven", "rhino", "salmon", "seal", "stoat", "swan",
	"tiger", "toucan", "viper", "walrus", "wolf", "wombat",
}

// Generate returns a fresh petname like "wandering-fox-3a4b". The 4-hex
// suffix makes collisions vanishingly unlikely without making the name
// hard to read aloud.
func Generate() string {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// rand.Read returning an error is essentially impossible on the
		// platforms we run on; if it does, fall back to a static suffix
		// rather than panicking — the caller can deduplicate.
		return fmt.Sprintf("%s-%s-0000", adjectives[0], animals[0])
	}
	adj := adjectives[int(buf[0])%len(adjectives)]
	animal := animals[int(buf[1])%len(animals)]
	suffix := hex.EncodeToString(buf[1:])
	return fmt.Sprintf("%s-%s-%s", adj, animal, suffix)
}
