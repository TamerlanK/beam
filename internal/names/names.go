package names

import "math/rand/v2"

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
