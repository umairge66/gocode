// Package planmode holds plan mode's session state and texts: the session plan
// file (its directory, word slug and contents), the plan-mode system reminders
// injected into the conversation, the EnterPlanMode / ExitPlanMode tool texts,
// and the Tracker that decides which reminder a turn carries.
//
// Plan mode is the read-only "explore and design first" permission mode. While
// it is on, the model may only take read-only actions plus writing its plan to
// the session plan file (<config home>/plans/<slug>.md, or the project-relative
// plansDirectory setting); it ends its planning by calling ExitPlanMode, which
// shows the plan to the user for approval.
//
// Everything here is local: the plan file lives on disk and nothing is sent
// anywhere except as part of the model conversation.
package planmode

import (
	"errors"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DirName is the plans folder under the config home.
const DirName = "plans"

// Plans locates and reads the session's plan files. The zero value (no Dir or
// Slug) has no plan file: Path returns "" and Read reports no plan.
type Plans struct {
	// Dir is the plans directory.
	Dir string
	// Slug is the session's plan slug; the main plan file is <Dir>/<Slug>.md.
	Slug string
}

// NewPlans returns the plan files of a session: the plans directory is
// plansDirectory resolved against the project root when it is set and stays
// inside that root, else <configHome>/plans; the slug derives from sessionID.
// It returns nil when neither a config home nor a usable plansDirectory is
// known.
func NewPlans(configHome, projectRoot, plansDirectory, sessionID string) *Plans {
	dir := ""
	if pd := strings.TrimSpace(plansDirectory); pd != "" && projectRoot != "" {
		if d, ok := withinRoot(projectRoot, pd); ok {
			dir = d
		}
	}
	if dir == "" && configHome != "" {
		dir = filepath.Join(configHome, DirName)
	}
	if dir == "" {
		return nil
	}
	return &Plans{Dir: dir, Slug: Slug(sessionID)}
}

// withinRoot resolves rel against root and reports whether the result stays
// inside root. A path containing a backslash is refused off Windows.
func withinRoot(root, rel string) (string, bool) {
	if filepath.Separator != '\\' && strings.Contains(rel, `\`) {
		return "", false
	}
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	r := filepath.Clean(root)
	if p != r && !strings.HasPrefix(p, r+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}

// Path returns the plan file path: <Dir>/<Slug>.md for the main session, or
// <Dir>/<Slug>-agent-<agentID>.md for a subagent. "" when there is no plan
// file location.
func (p *Plans) Path(agentID string) string {
	if p == nil || p.Dir == "" || p.Slug == "" {
		return ""
	}
	if agentID == "" {
		return filepath.Join(p.Dir, p.Slug+".md")
	}
	return filepath.Join(p.Dir, p.Slug+"-agent-"+agentID+".md")
}

// Read returns the plan file's contents and whether a plan file exists.
func (p *Plans) Read(agentID string) (string, bool) {
	path := p.Path(agentID)
	if path == "" {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Exists reports whether the plan file exists.
func (p *Plans) Exists(agentID string) bool {
	path := p.Path(agentID)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// EnsureDir creates the plans directory (mode 0700) so the model's first
// Write of the plan file does not fail. An existing directory is fine.
func (p *Plans) EnsureDir() error {
	if p == nil || p.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// Slug returns the three-word plan slug ("<adjective>-<verb>-<noun>") for a
// seed. The same seed (a session id) always yields the same slug, so a resumed
// session finds its plan file again; an empty seed yields a fixed slug.
func Slug(seed string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	v := h.Sum64()
	a := slugAdjectives[v%uint64(len(slugAdjectives))]
	v /= uint64(len(slugAdjectives))
	g := slugVerbs[v%uint64(len(slugVerbs))]
	v /= uint64(len(slugVerbs))
	n := slugNouns[v%uint64(len(slugNouns))]
	return a + "-" + g + "-" + n
}

var slugAdjectives = []string{
	"abundant", "ancient", "bright", "calm", "cheerful", "clever", "cozy", "curious",
	"dapper", "dazzling", "deep", "delightful", "eager", "elegant", "enchanted",
	"fancy", "fluffy", "gentle", "gleaming", "golden", "graceful", "happy", "hidden",
	"humble", "jolly", "joyful", "keen", "kind", "lively", "lovely", "lucky",
	"luminous", "magical", "majestic", "mellow", "merry", "mighty", "misty", "noble",
	"peaceful", "playful", "polished", "precious", "proud", "quiet", "quirky",
	"radiant", "rosy", "serene", "shiny", "silly", "sleepy", "smooth", "snazzy",
	"snug", "soft", "sparkling", "spicy", "splendid", "sprightly", "starry",
	"steady", "sunny", "swift", "tender", "tidy", "toasty", "tranquil", "twinkly",
	"valiant", "vast", "velvet", "vivid", "warm", "whimsical", "wild", "wise",
	"witty", "wondrous", "zany", "zesty", "zippy", "breezy", "bubbly", "cosmic",
	"crystalline", "dreamy", "ethereal", "fizzy", "glowing", "groovy", "jazzy",
	"moonlit", "mossy", "nifty", "peppy", "shimmering", "snappy", "swirling",
	"atomic", "binary", "cached", "compiled", "concurrent", "declarative",
	"dynamic", "functional", "generic", "immutable", "iterative", "lazy", "linear",
	"modular", "nested", "parallel", "pure", "reactive", "recursive", "robust",
	"scalable", "sequential", "stateless", "structured", "typed", "virtual",
}

var slugVerbs = []string{
	"baking", "beaming", "booping", "bouncing", "brewing", "bubbling", "chasing",
	"churning", "coalescing", "conjuring", "cooking", "crafting", "crunching",
	"cuddling", "dancing", "dazzling", "discovering", "doodling", "dreaming",
	"drifting", "enchanting", "exploring", "finding", "floating", "fluttering",
	"foraging", "forging", "frolicking", "gathering", "giggling", "gliding",
	"greeting", "growing", "hatching", "herding", "honking", "hopping", "hugging",
	"humming", "imagining", "inventing", "jingling", "juggling", "jumping",
	"kindling", "knitting", "launching", "leaping", "mapping", "marinating",
	"meandering", "mixing", "moseying", "munching", "napping", "nibbling",
	"noodling", "orbiting", "painting", "percolating", "petting", "plotting",
	"pondering", "popping", "prancing", "purring", "puzzling", "questing",
	"riding", "roaming", "rolling", "sauteeing", "scribbling", "seeking",
	"shimmying", "singing", "skipping", "sleeping", "snacking", "sniffing",
	"snuggling", "soaring", "sparking", "spinning", "splashing", "sprouting",
	"squishing", "stargazing", "stirring", "strolling", "swimming", "swinging",
	"tickling", "tinkering", "toasting", "tumbling", "twirling", "waddling",
	"wandering", "watching", "weaving", "whistling", "wibbling", "wiggling",
	"wishing", "wobbling", "wondering", "yawning", "zooming",
}

var slugNouns = []string{
	"aurora", "avalanche", "blossom", "breeze", "brook", "bubble", "canyon",
	"cascade", "cloud", "clover", "comet", "coral", "cosmos", "creek", "crescent",
	"crystal", "dawn", "dewdrop", "dusk", "eclipse", "ember", "feather", "fern",
	"firefly", "flame", "flurry", "fog", "forest", "frost", "galaxy", "garden",
	"glacier", "glade", "grove", "harbor", "horizon", "island", "lagoon", "lake",
	"leaf", "lightning", "meadow", "meteor", "mist", "moon", "moonbeam", "mountain",
	"nebula", "nova", "ocean", "orbit", "pebble", "petal", "pine", "planet", "pond",
	"quasar", "rain", "rainbow", "reef", "ripple", "river", "shore", "sky",
	"snowflake", "spark", "spring", "star", "stardust", "starlight", "storm",
	"stream", "summit", "sun", "sunbeam", "sunrise", "sunset", "thunder", "tide",
	"twilight", "valley", "volcano", "waterfall", "wave", "willow", "wind",
	"alpaca", "axolotl", "badger", "bear", "beaver", "bee", "bunny", "cat",
	"chipmunk", "crab", "crane", "deer", "dolphin", "dove", "dragon", "dragonfly",
	"eagle", "falcon", "finch", "flamingo", "fox", "frog", "giraffe", "goose",
	"hamster", "heron", "hummingbird", "jellyfish", "kitten", "koala", "ladybug",
	"lemur", "llama", "lynx", "meerkat", "narwhal", "newt", "octopus", "otter",
	"owl", "panda", "parrot", "peacock", "pelican", "penguin", "phoenix",
	"platypus", "puffin", "quokka", "rabbit", "raccoon", "raven", "robin",
	"seahorse", "sloth", "sparrow", "squirrel", "swan", "tiger", "toucan",
	"turtle", "unicorn", "walrus", "whale", "wolf", "wombat", "wren", "zebra",
	"acorn", "anchor", "balloon", "beacon", "biscuit", "candle", "castle", "cookie",
	"crayon", "cupcake", "kite", "lantern", "lighthouse", "marble", "muffin",
	"origami", "pancake", "pinwheel", "pixel", "prism", "puzzle", "quill", "rocket",
	"teacup", "teapot", "treehouse", "waffle", "whistle", "zephyr",
}
