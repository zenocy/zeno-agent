package synth

import (
	"bufio"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/sirupsen/logrus"
)

//go:embed templates/*.tmpl templates/_voice.md templates/_voice_short.md
var embeddedPrompts embed.FS

// PromptSet is the parsed prompt artifacts the runner needs at every synth.
// Voice is the raw _voice.md text inlined into briefing.tmpl via {{.Voice}}.
// VoiceShort is the compressed 5-bullet form inlined into cards/reactive
// prompts via {{.VoiceShort}} — saves ~1100 tokens per call where the full
// canon is overkill (cards have their own field-rules block; reactive
// produces one short card).
//
// V2.3.0 P2: StateVoice and StateBias hold per-state overlays parsed from
// `_voice.md`'s `## State: <name>` and `## Cards bias: <name>` sections.
// Briefing renders StateVoice[deps.State]; cards renders StateBias[deps.State].
// Missing-state lookups fall back to StateMorningCalm at parse time.
type PromptSet struct {
	Voice          string
	VoiceShort     string
	CardsSystem    *template.Template
	BriefingSystem *template.Template
	Reactive       *template.Template
	Converse       *template.Template // V2.10: card-conversation prompt

	// V2.3 additions:
	StateVoice map[State]string // keyed by State; rendered in briefing.tmpl
	StateBias  map[State]string // keyed by State; rendered in cards_system.tmpl

	// V2.13.0: assistant register block. Read from `## Register: assistant`
	// in `_voice.md` and inlined into the WhatsApp draft prompt only when
	// the user has configured an assistant name. Parsed once at boot;
	// callers must treat the string as immutable.
	AssistantRegister string
}

// LoadPrompts loads the voice rules + templates. If dir is non-empty, files
// are read from disk (for prompt iteration); otherwise the binary's embedded
// copies are used. Disk wins so the replay CLI can iterate prompts without a
// rebuild.
func LoadPrompts(dir string) (*PromptSet, error) {
	src := promptSource(dir)

	voiceBytes, err := fs.ReadFile(src, "_voice.md")
	if err != nil {
		return nil, fmt.Errorf("read voice rules: %w", err)
	}

	voiceShortBytes, err := fs.ReadFile(src, "_voice_short.md")
	if err != nil {
		return nil, fmt.Errorf("read voice-short rules: %w", err)
	}

	cardsBytes, err := fs.ReadFile(src, "cards_system.tmpl")
	if err != nil {
		return nil, fmt.Errorf("read cards template: %w", err)
	}
	briefingBytes, err := fs.ReadFile(src, "briefing.tmpl")
	if err != nil {
		return nil, fmt.Errorf("read briefing template: %w", err)
	}
	reactiveBytes, err := fs.ReadFile(src, "reactive.tmpl")
	if err != nil {
		return nil, fmt.Errorf("read reactive template: %w", err)
	}
	converseBytes, err := fs.ReadFile(src, "converse_system.tmpl")
	if err != nil {
		return nil, fmt.Errorf("read converse template: %w", err)
	}

	// Shared funcs available across templates.
	sharedFuncs := template.FuncMap{
		"add":  func(a, b int) int { return a + b },
		"join": strings.Join,
	}

	cards, err := template.New("cards_system").Funcs(sharedFuncs).Parse(string(cardsBytes))
	if err != nil {
		return nil, fmt.Errorf("parse cards template: %w", err)
	}
	briefing, err := template.New("briefing").Funcs(sharedFuncs).Parse(string(briefingBytes))
	if err != nil {
		return nil, fmt.Errorf("parse briefing template: %w", err)
	}
	reactive, err := template.New("reactive").Funcs(sharedFuncs).Parse(string(reactiveBytes))
	if err != nil {
		return nil, fmt.Errorf("parse reactive template: %w", err)
	}
	converse, err := template.New("converse").Funcs(sharedFuncs).Parse(string(converseBytes))
	if err != nil {
		return nil, fmt.Errorf("parse converse template: %w", err)
	}

	stateVoice, stateBias, err := parseStateBlocks(string(voiceBytes))
	if err != nil {
		return nil, fmt.Errorf("parse state blocks: %w", err)
	}

	assistantRegister := parseAssistantRegister(string(voiceBytes))

	return &PromptSet{
		Voice:             string(voiceBytes),
		VoiceShort:        string(voiceShortBytes),
		CardsSystem:       cards,
		BriefingSystem:    briefing,
		Reactive:          reactive,
		Converse:          converse,
		StateVoice:        stateVoice,
		StateBias:         stateBias,
		AssistantRegister: assistantRegister,
	}, nil
}

// parseAssistantRegister returns the body of the `## Register: assistant`
// block from `_voice.md`, with leading/trailing whitespace trimmed.
// Empty string when the block is missing — caller treats that as "no
// register block to inline" and the WhatsApp draft prompt falls back to
// inline persona instructions only.
func parseAssistantRegister(voice string) string {
	const heading = "## Register: assistant"
	idx := strings.Index(voice, heading)
	if idx < 0 {
		return ""
	}
	rest := voice[idx+len(heading):]
	// Body runs until next "## " heading or "---" separator (matches
	// parseStateBlocks semantics).
	end := len(rest)
	scanner := bufio.NewScanner(strings.NewReader(rest))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	pos := 0
	for scanner.Scan() {
		line := scanner.Text()
		// Skip the heading's own newline at offset 0.
		if pos > 0 {
			if strings.HasPrefix(line, "## ") || strings.TrimSpace(line) == "---" {
				end = pos
				break
			}
		}
		pos += len(line) + 1
	}
	body := rest[:end]
	body = strings.TrimPrefix(body, "\n")
	return strings.TrimSpace(body)
}

// promptSource returns the fs.FS to load prompts from. If dir is set and
// readable, it wraps the directory; otherwise it returns the embedded FS
// rooted at templates/.
func promptSource(dir string) fs.FS {
	if dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "_voice.md")); err == nil {
			return os.DirFS(dir)
		}
	}
	sub, err := fs.Sub(embeddedPrompts, "templates")
	if err != nil {
		// Should never happen — embed.FS is built at compile time.
		panic(fmt.Sprintf("synth: embedded templates missing: %v", err))
	}
	return sub
}

// stateBlockHeading captures `## State: <name>` and `## Cards bias: <name>`
// markdown headings out of `_voice.md`. The body for each block is everything
// up to (but not including) the next `## ` heading. `<name>` must match one
// of the V2.3 closed-set State constants; unknown names are skipped.
var stateBlockHeading = regexp.MustCompile(`^##\s+(State|Cards bias):\s+([a-z_]+)\s*$`)

// parseStateBlocks scans voice for `## State: <name>` and `## Cards bias:
// <name>` sections and returns two maps keyed by State. Missing states fall
// back to StateMorningCalm's body so callers always have a non-empty overlay
// to render. A missing morning_calm block is a hard error: there is no
// fallback target.
//
// The parser stops a block's body at the next `## ` heading (any kind) or at
// the next `---` separator on a line by itself, whichever comes first. This
// prevents the trailing `---` separator that closes the state-blocks region
// from leaking into the last block's body.
func parseStateBlocks(voice string) (stateVoice, stateBias map[State]string, err error) {
	stateVoice = make(map[State]string)
	stateBias = make(map[State]string)

	type kind int
	const (
		kindNone kind = iota
		kindState
		kindBias
	)

	var (
		curKind  kind
		curState State
		curBody  []string
	)

	flush := func() {
		if curKind == kindNone || curState == "" {
			return
		}
		body := strings.TrimSpace(strings.Join(curBody, "\n"))
		switch curKind {
		case kindState:
			stateVoice[curState] = body
		case kindBias:
			stateBias[curState] = body
		}
	}

	scanner := bufio.NewScanner(strings.NewReader(voice))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		// Treat any `## ` heading or a `---` separator as a block terminator.
		isHeading := strings.HasPrefix(line, "## ")
		isSeparator := strings.TrimSpace(line) == "---"

		if isHeading || isSeparator {
			flush()
			curKind = kindNone
			curState = ""
			curBody = nil

			if isHeading {
				if m := stateBlockHeading.FindStringSubmatch(line); m != nil {
					name := State(m[2])
					if !name.IsValid() {
						// Unknown state name (typo or new-state-not-in-the-closed-set).
						// Skip and continue scanning.
						continue
					}
					curState = name
					switch m[1] {
					case "State":
						curKind = kindState
					case "Cards bias":
						curKind = kindBias
					}
				}
			}
			continue
		}

		if curKind != kindNone {
			curBody = append(curBody, line)
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan voice: %w", err)
	}

	// morning_calm is the fallback target — its absence is fatal.
	if _, ok := stateVoice[StateMorningCalm]; !ok {
		return nil, nil, fmt.Errorf("missing `## State: morning_calm` block in _voice.md")
	}
	if _, ok := stateBias[StateMorningCalm]; !ok {
		return nil, nil, fmt.Errorf("missing `## Cards bias: morning_calm` block in _voice.md")
	}

	// Fill in any missing state blocks with the morning_calm body so callers
	// always have a non-empty overlay to render. WARN once per missing state.
	allStates := []State{StateMorningCalm, StatePreMeeting, StateDeepWork, StateEndOfDay, StateMessageInject}
	for _, s := range allStates {
		if _, ok := stateVoice[s]; !ok {
			logrus.WithField("state", string(s)).
				Warn("synth: missing `## State` block in _voice.md — falling back to morning_calm")
			stateVoice[s] = stateVoice[StateMorningCalm]
		}
		if _, ok := stateBias[s]; !ok {
			logrus.WithField("state", string(s)).
				Warn("synth: missing `## Cards bias` block in _voice.md — falling back to morning_calm")
			stateBias[s] = stateBias[StateMorningCalm]
		}
	}

	return stateVoice, stateBias, nil
}
