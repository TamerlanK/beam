package names

import (
	"math/rand/v2"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/TamerlanK/beam/internal/protocol"
)

var adjectives = []string{
	"Amber", "Azure", "Bold", "Brave", "Bright", "Calm", "Clever", "Coral",
	"Cosmic", "Crimson", "Daring", "Dapper", "Eager", "Electric", "Emerald",
	"Fierce", "Gentle", "Gilded", "Golden", "Happy", "Indigo", "Ivory",
	"Jade", "Jolly", "Lively", "Lucky", "Lunar", "Mellow", "Mighty", "Misty",
	"Noble", "Olive", "Onyx", "Pearl", "Plucky", "Polar", "Proud", "Purple",
	"Quick", "Royal", "Ruby", "Rustic", "Scarlet", "Silent", "Silver",
	"Snowy", "Solar", "Sunny", "Swift", "Teal", "Velvet", "Vivid", "Wild",
	"Witty", "Zesty",
}

type animal struct {
	name  string
	emoji string
}

var animals = []animal{
	{"Badger", "🦡"}, {"Bear", "🐻"}, {"Beaver", "🦫"}, {"Bee", "🐝"},
	{"Bison", "🦬"}, {"Butterfly", "🦋"}, {"Cat", "🐱"}, {"Crab", "🦀"},
	{"Deer", "🦌"}, {"Dolphin", "🐬"}, {"Dragon", "🐉"}, {"Duck", "🦆"},
	{"Eagle", "🦅"}, {"Falcon", "🦅"}, {"Flamingo", "🦩"}, {"Fox", "🦊"},
	{"Frog", "🐸"}, {"Hawk", "🦅"}, {"Hedgehog", "🦔"}, {"Heron", "🐦"},
	{"Koala", "🐨"}, {"Lion", "🦁"}, {"Lizard", "🦎"}, {"Llama", "🦙"},
	{"Lynx", "🐈"}, {"Marmot", "🐹"}, {"Narwhal", "🐋"}, {"Octopus", "🐙"},
	{"Otter", "🦦"}, {"Owl", "🦉"}, {"Panda", "🐼"}, {"Panther", "🐆"},
	{"Parrot", "🦜"}, {"Penguin", "🐧"}, {"Rabbit", "🐰"}, {"Raccoon", "🦝"},
	{"Raven", "🐦‍⬛"}, {"Seal", "🦭"}, {"Shark", "🦈"}, {"Sparrow", "🐦"},
	{"Squid", "🦑"}, {"Swan", "🦢"}, {"Tiger", "🐯"}, {"Toucan", "🦤"},
	{"Turtle", "🐢"}, {"Whale", "🐳"}, {"Wolf", "🐺"}, {"Wombat", "🐻"},
}

func Random() (name, emoji string) {
	a := adjectives[rand.IntN(len(adjectives))]
	an := animals[rand.IntN(len(animals))]
	return a + " " + an.name, an.emoji
}

func CleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > protocol.MaxNameRunes {
		rs := []rune(s)
		s = strings.TrimSpace(string(rs[:protocol.MaxNameRunes]))
	}
	return s
}

func CleanEmoji(s string) string {
	if s == "" || len(s) > protocol.MaxEmojiBytes || !utf8.ValidString(s) {
		return ""
	}
	for _, r := range s {
		if r < 0x80 || unicode.IsSpace(r) || unicode.IsControl(r) {
			return ""
		}
	}
	return s
}
