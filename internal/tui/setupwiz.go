package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configtools"
	"github.com/TedHaley/cheep/internal/provider"
)

// The setup wizard ("setupwiz" overlay) is a keyboard-driven configurator. It
// runs discovery (local servers + API keys), lets the user pick an orchestrator
// and tag executors, and — when nothing discovered fits — offers a manual form.
// It opens automatically on first launch and via /config.

type wizCandidate struct {
	provider, endpoint, model, key, src string
	local                               bool
}

func (c wizCandidate) where() string {
	switch {
	case c.provider == "anthropic":
		if c.key == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
			return "Anthropic · needs key (you'll paste it)"
		}
		if c.src != "" {
			return "Anthropic · " + c.src
		}
		return "Anthropic"
	case c.local:
		return c.endpoint + " · local"
	case c.src != "":
		return c.endpoint + " · " + c.src
	}
	return c.endpoint
}

type wizState struct {
	mode    string // "pick" | "manual"
	loading bool
	cands   []wizCandidate
	extra   []string // key names found but not directly usable
	cursor  int
	orch    int // index into cands, -1 = none chosen
	execs   map[int]bool

	fields     []textinput.Model // manual form inputs
	focus      int
	manualProv int // selected provider in the manual provider list
	err        string

	keyInput textinput.Model // "key" mode: paste an Anthropic key for a keyless pick
	pending  config.Config   // config awaiting the pasted key
}

// manualProviders are the cloud backends offered in the manual setup list. All
// but Anthropic are OpenAI-compatible; their endpoints are prefilled.
type manualProvider struct{ label, provider, endpoint string }

var manualProviders = []manualProvider{
	{"Anthropic (Claude)", "anthropic", ""},
	{"OpenAI", "openai", "https://api.openai.com/v1"},
	{"Grok (xAI)", "openai", "https://api.x.ai/v1"},
	{"DeepSeek", "openai", "https://api.deepseek.com/v1"},
	{"Groq", "openai", "https://api.groq.com/openai/v1"},
	{"OpenRouter", "openai", "https://openrouter.ai/api/v1"},
	{"Mistral", "openai", "https://api.mistral.ai/v1"},
	{"Custom (OpenAI-compatible)", "openai", ""},
}

// chatModel filters out models that can't act as an orchestrator/executor
// (embeddings, speech, rerankers, …) so they don't clutter the picker.
func chatModel(name string) bool {
	n := strings.ToLower(name)
	for _, bad := range []string{"embed", "whisper", "tts", "rerank", "moderation", "clip", "diffusion"} {
		if strings.Contains(n, bad) {
			return false
		}
	}
	return true
}

func modelHint(p manualProvider) string {
	switch p.label {
	case "Anthropic (Claude)":
		return "model id, e.g. claude-sonnet-4-6"
	case "OpenAI":
		return "model id, e.g. gpt-4o"
	case "Grok (xAI)":
		return "model id, e.g. grok-2-latest"
	case "DeepSeek":
		return "model id, e.g. deepseek-chat"
	}
	return "model id"
}

func newWizState() wizState {
	return wizState{mode: "pick", loading: true, orch: -1, execs: map[int]bool{}}
}

type wizMsg struct {
	cands []wizCandidate
	extra []string
}

// openWizard opens the discovery configurator and starts the scan.
func (m model) openWizard() (tea.Model, tea.Cmd) {
	m.overlay = "setupwiz"
	m.wiz = newWizState()
	return m, wizDiscoverCmd()
}

// wizDiscoverCmd scans for local servers and API keys, and (for cloud keys)
// lists their models, off the UI goroutine. It surfaces only what actually
// exists; cloud providers without a discovered key are added via manual entry.
func wizDiscoverCmd() tea.Cmd {
	return func() tea.Msg {
		servers, keys := configtools.Discover()
		var cands []wizCandidate
		for _, srv := range servers {
			for _, mdl := range srv.Models {
				if !chatModel(mdl) {
					continue
				}
				cands = append(cands, wizCandidate{provider: "openai", endpoint: srv.Endpoint, model: mdl, local: true})
			}
		}
		var extra []string
		for _, k := range keys {
			switch {
			case k.Provider == "anthropic":
				cands = append(cands, wizCandidate{provider: "anthropic", model: "claude-sonnet-4-6", key: k.Value, src: k.Name})
			case k.Provider == "openai" && k.Endpoint != "":
				base, models, err := provider.DiscoverModels(k.Endpoint, k.Value)
				if err != nil || len(models) == 0 {
					extra = append(extra, k.Name)
					continue
				}
				if len(models) > 8 { // keep the picker readable
					models = models[:8]
				}
				for _, mdl := range models {
					if !chatModel(mdl) {
						continue
					}
					cands = append(cands, wizCandidate{provider: "openai", endpoint: base, model: mdl, key: k.Value, src: k.Name})
				}
			default:
				extra = append(extra, k.Name)
			}
		}
		return wizMsg{cands: cands, extra: extra}
	}
}

func (m model) updateWiz(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.wiz.mode {
	case "manual":
		return m.updateWizManual(msg)
	case "mfields":
		return m.updateWizManualFields(msg)
	case "key":
		return m.updateWizKey(msg)
	}
	w := &m.wiz
	if w.loading {
		if s := msg.String(); s == "esc" || s == "ctrl+c" {
			return m.closeWizard()
		}
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		return m.closeWizard()
	case "up", "k":
		if w.cursor > 0 {
			w.cursor--
		}
	case "down", "j":
		if w.cursor < len(w.cands)-1 {
			w.cursor++
		}
	case "o":
		if len(w.cands) > 0 {
			w.orch = w.cursor
			w.err = ""
		}
	case "e", " ":
		if len(w.cands) > 0 {
			if w.execs[w.cursor] {
				delete(w.execs, w.cursor)
			} else {
				w.execs[w.cursor] = true
			}
			w.err = ""
		}
	case "m":
		return m.enterManual()
	case "enter":
		return m.saveWizard()
	}
	return m, nil
}

// enterManual opens the manual provider list (Anthropic, OpenAI, Grok, …).
func (m model) enterManual() (tea.Model, tea.Cmd) {
	w := &m.wiz
	w.mode = "manual"
	w.manualProv = 0
	w.err = ""
	return m, nil
}

func (m model) updateWizManual(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	w := &m.wiz
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		w.mode, w.err = "pick", ""
	case "up", "k":
		if w.manualProv > 0 {
			w.manualProv--
		}
	case "down", "j":
		if w.manualProv < len(manualProviders)-1 {
			w.manualProv++
		}
	case "enter":
		return m.enterManualFields()
	}
	return m, nil
}

// enterManualFields shows the credential inputs for the chosen provider, with the
// endpoint prefilled.
func (m model) enterManualFields() (tea.Model, tea.Cmd) {
	w := &m.wiz
	p := manualProviders[w.manualProv]
	mk := func(placeholder, val string) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.SetValue(val)
		ti.Width = max(20, m.w-26)
		return ti
	}
	w.mode = "mfields"
	w.err = ""
	w.focus = 0
	mdl := mk(modelHint(p), "")
	key := mk("API key", "")
	key.EchoMode = textinput.EchoPassword
	if p.provider == "anthropic" {
		w.fields = []textinput.Model{mdl, key}
	} else {
		w.fields = []textinput.Model{mk("https://… endpoint", p.endpoint), mdl, key}
	}
	w.fields[0].Focus()
	return m, textinput.Blink
}

func (m model) updateWizManualFields(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	w := &m.wiz
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		w.mode, w.err = "manual", "" // back to the provider list
		return m, nil
	case "enter":
		return m.saveManual()
	case "tab", "down":
		w.focus = (w.focus + 1) % len(w.fields)
	case "shift+tab", "up":
		w.focus = (w.focus - 1 + len(w.fields)) % len(w.fields)
	default:
		var cmd tea.Cmd
		w.fields[w.focus], cmd = w.fields[w.focus].Update(msg)
		return m, cmd
	}
	for i := range w.fields {
		if i == w.focus {
			w.fields[i].Focus()
		} else {
			w.fields[i].Blur()
		}
	}
	return m, nil
}

func (m model) saveManual() (tea.Model, tea.Cmd) {
	w := &m.wiz
	p := manualProviders[w.manualProv]
	var endpoint, mdl, key string
	if p.provider == "anthropic" {
		mdl = strings.TrimSpace(w.fields[0].Value())
		key = strings.TrimSpace(w.fields[1].Value())
	} else {
		endpoint = strings.TrimSpace(w.fields[0].Value())
		mdl = strings.TrimSpace(w.fields[1].Value())
		key = strings.TrimSpace(w.fields[2].Value())
	}
	switch {
	case mdl == "":
		w.err = "a model id is required"
		return m, nil
	case p.provider == "openai" && endpoint == "":
		w.err = "an endpoint is required"
		return m, nil
	case p.provider == "anthropic" && key == "" && os.Getenv("ANTHROPIC_API_KEY") == "":
		w.err = "an API key is required for Anthropic"
		return m, nil
	}
	if p.provider == "anthropic" && key != "" {
		_ = config.SetKey("ANTHROPIC_API_KEY", key)
	}
	cfg := config.Config{Orchestrator: config.Agent{Provider: p.provider, Endpoint: endpoint, Model: mdl, APIKey: key}}
	return m.applyWizConfig(cfg, "manual setup saved · "+p.label)
}

func (m model) saveWizard() (tea.Model, tea.Cmd) {
	w := &m.wiz
	if w.orch < 0 || w.orch >= len(w.cands) {
		w.err = "pick an orchestrator first (move with ↑/↓, press o)"
		return m, nil
	}
	// cheep needs both roles — require at least one executor. The orchestrator's
	// own model may be reused as an executor (tag the same row with e).
	execCount := 0
	for i := range w.cands {
		if w.execs[i] {
			execCount++
		}
	}
	if execCount == 0 {
		w.err = "add at least one executor (press e) — cheep runs an orchestrator + executors"
		return m, nil
	}
	oc := w.cands[w.orch]
	cfg := config.Config{Orchestrator: config.Agent{Provider: oc.provider, Endpoint: oc.endpoint, Model: oc.model, APIKey: oc.key}}
	n := 1
	for i := range w.cands {
		if !w.execs[i] {
			continue
		}
		e := w.cands[i]
		cfg.Executors = append(cfg.Executors, config.Agent{
			Name: fmt.Sprintf("executor-%d", n), Provider: e.provider,
			Endpoint: e.endpoint, Model: e.model, APIKey: e.key,
		})
		n++
	}
	// Claude chosen but no key on hand — ask the user to paste it.
	if oc.provider == "anthropic" && oc.key == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return m.enterKeyMode(cfg)
	}
	return m.applyWizConfig(cfg, "configured from discovered services")
}

func (m model) enterKeyMode(pending config.Config) (tea.Model, tea.Cmd) {
	w := &m.wiz
	w.pending = pending
	w.mode = "key"
	w.err = ""
	ti := textinput.New()
	ti.Placeholder = "sk-ant-…"
	ti.EchoMode = textinput.EchoPassword
	ti.Width = max(24, m.w-26)
	ti.Focus()
	w.keyInput = ti
	return m, textinput.Blink
}

func (m model) updateWizKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	w := &m.wiz
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		w.mode, w.err = "pick", ""
		return m, nil
	case "enter":
		key := strings.TrimSpace(w.keyInput.Value())
		if key == "" {
			w.err = "paste your Anthropic API key (or esc to go back)"
			return m, nil
		}
		if err := config.SetKey("ANTHROPIC_API_KEY", key); err != nil {
			w.err = "save failed: " + err.Error()
			return m, nil
		}
		w.pending.Orchestrator.APIKey = key
		return m.applyWizConfig(w.pending, "saved your Anthropic key · keeping Claude")
	}
	var cmd tea.Cmd
	w.keyInput, cmd = w.keyInput.Update(msg)
	return m, cmd
}

// applyWizConfig persists the new config, rebuilds the session, and refreshes the
// banner — shared by the discovered-pick and manual paths.
func (m model) applyWizConfig(cfg config.Config, okMsg string) (tea.Model, tea.Cmd) {
	if err := config.Save(cfg); err != nil {
		m.wiz.err = "save failed: " + err.Error()
		return m, nil
	}
	if c, err := config.Load(); err == nil {
		m.cfg = c
	}
	m.keepTabs = m.cfg.KeepTabs
	m.overlay = ""
	(&m).rebuild(true)

	banner := welcomeLines(m.cfg, nil)
	if len(m.tabs[0].lines) == m.welcomeLen { // banner untouched — replace it
		m.tabs[0].lines = banner
	} else { // mid-session — append a fresh banner
		m.tabs[0].lines = append(append(m.tabs[0].lines, ""), banner...)
	}
	m.welcomeLen = len(m.tabs[0].lines)
	if m.buildErr != nil {
		m.tabs[0].lines = append(m.tabs[0].lines,
			errSt.Render("✗ ")+errText(m.buildErr)+hintSt.Render("  ·  fix with /config"))
	}
	m.footer = okMsg + " · orchestrator " + m.cfg.Orchestrator.Model
	m.active, m.follow = 0, true
	(&m).syncViewport()
	return m, probeCmd(m.cfg)
}

func (m model) closeWizard() (tea.Model, tea.Cmd) {
	m.overlay = ""
	if m.session == nil {
		m.footer = "setup cancelled — run /config to set up cheep"
	}
	(&m).syncViewport()
	return m, nil
}

func (m model) viewWiz() string {
	title := lipgloss.NewStyle().Bold(true).Render("Set up cheep")
	switch m.wiz.mode {
	case "manual":
		return m.viewWizManual(title)
	case "mfields":
		return m.viewWizManualFields(title)
	case "key":
		return m.viewWizKey(title)
	}
	w := m.wiz
	if w.loading {
		body := title + "\n\n  scanning for local servers and API keys…"
		return m.place(ovBox.Padding(1, 3).Render(body))
	}
	lines := []string{title, hintSt.Render("Pick an orchestrator; optionally tag executors to delegate to."), ""}
	if len(w.cands) == 0 {
		lines = append(lines, "  No local servers or usable API keys were found.",
			hintSt.Render("  Start a local model (Ollama, LM Studio, …) or add an API key — or enter one manually."), "")
	} else {
		for i, c := range w.cands {
			cur := "  "
			if i == w.cursor {
				cur = todoProgSt.Render("▸ ")
			}
			tags := ""
			if i == w.orch {
				tags += " " + okSt.Render("[orchestrator]")
			}
			if w.execs[i] {
				tags += " " + todoProgSt.Render("[executor]")
			}
			lines = append(lines, cur+c.model+hintSt.Render("  "+c.where())+tags)
		}
	}
	if len(w.extra) > 0 {
		lines = append(lines, "", hintSt.Render("keys found (models unlisted): "+strings.Join(w.extra, ", ")))
	}
	if w.err != "" {
		lines = append(lines, "", errSt.Render("✗ "+w.err))
	}

	// Live setup summary + step-by-step guidance.
	orchName := "—"
	if w.orch >= 0 && w.orch < len(w.cands) {
		orchName = short(w.cands[w.orch].model, 28)
	}
	var execNames []string
	for i := range w.cands {
		if w.execs[i] {
			execNames = append(execNames, short(w.cands[i].model, 20))
		}
	}
	// Suggest an executor: prefer a model other than the orchestrator, else reuse it.
	suggest := ""
	for i := range w.cands {
		if i != w.orch {
			suggest = short(w.cands[i].model, 22)
			break
		}
	}
	if suggest == "" && w.orch >= 0 {
		suggest = short(w.cands[w.orch].model, 16) + " (same model)"
	}
	execPart := "none"
	if len(execNames) > 0 {
		execPart = strings.Join(execNames, ", ")
	}
	lines = append(lines, "", hintSt.Render("setup → orchestrator ")+orchName+hintSt.Render("  ·  executors ")+execPart)
	switch {
	case w.orch < 0:
		lines = append(lines, hintSt.Render("step 1 — press o to choose the orchestrator"))
	case len(execNames) == 0:
		lines = append(lines, hintSt.Render("step 2 — press e to add an executor (e.g. "+suggest+")"))
	default:
		lines = append(lines, hintSt.Render("enter to save"))
	}

	lines = append(lines, "",
		hintSt.Render("↑/↓ move · o orchestrator · e executor · enter save · m manual · esc cancel"))
	return m.place(ovBox.Padding(1, 2).Render(strings.Join(lines, "\n")))
}

func (m model) viewWizKey(title string) string {
	w := m.wiz
	lines := []string{
		title + hintSt.Render("  ·  Anthropic API key"), "",
		"  " + w.pending.Orchestrator.Model + hintSt.Render("  needs an API key to run"), "",
		"  key  " + w.keyInput.View(),
	}
	if w.err != "" {
		lines = append(lines, "", errSt.Render("✗ "+w.err))
	}
	lines = append(lines, "",
		hintSt.Render("enter save · esc back · stored in ~/.cheep/keys.env (0600), active immediately"))
	return m.place(ovBox.Padding(1, 2).Render(strings.Join(lines, "\n")))
}

func (m model) viewWizManual(title string) string { // provider list
	w := m.wiz
	lines := []string{title + hintSt.Render("  ·  manual setup"),
		hintSt.Render("Choose a provider, then enter your credentials."), ""}
	for i, p := range manualProviders {
		cur := "  "
		if i == w.manualProv {
			cur = todoProgSt.Render("▸ ")
		}
		where := p.endpoint
		if where == "" {
			if p.provider == "anthropic" {
				where = "api.anthropic.com"
			} else {
				where = "you'll enter the endpoint"
			}
		}
		lines = append(lines, cur+p.label+hintSt.Render("  "+where))
	}
	if w.err != "" {
		lines = append(lines, "", errSt.Render("✗ "+w.err))
	}
	lines = append(lines, "",
		hintSt.Render("↑/↓ move · enter choose · esc back to discovered list"))
	return m.place(ovBox.Padding(1, 2).Render(strings.Join(lines, "\n")))
}

func (m model) viewWizManualFields(title string) string {
	w := m.wiz
	p := manualProviders[w.manualProv]
	labels := []string{"endpoint", "model", "api key"}
	if p.provider == "anthropic" {
		labels = []string{"model", "api key"}
	}
	lines := []string{title + hintSt.Render("  ·  "+p.label), ""}
	for i, f := range w.fields {
		marker := "  "
		if i == w.focus {
			marker = todoProgSt.Render("▸ ")
		}
		lines = append(lines, marker+fmt.Sprintf("%-9s ", labels[i])+f.View())
	}
	if w.err != "" {
		lines = append(lines, "", errSt.Render("✗ "+w.err))
	}
	lines = append(lines, "",
		hintSt.Render("tab / ↑↓ move · enter save · esc back to providers"))
	return m.place(ovBox.Padding(1, 2).Render(strings.Join(lines, "\n")))
}
