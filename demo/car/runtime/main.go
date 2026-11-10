// Command runtime is the car demo's scripted agent runtime. It reads the voice
// producer's stream (JSON lines on stdin), matches trigger phrases in each
// utterance to MCP tool calls, and makes those calls through attest-gateway,
// stamping params._meta["attest/subject"] with the utterance's subject.
//
// It stands in for the speech runtime of a real assistant: the component
// that knows who said which words. It, not a language model, is what stamps
// the subject; a model that could write that field could claim any speaker.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mad-deecent/attest/internal/decision"
	"github.com/mad-deecent/attest/internal/gateway"
)

// Trigger turns a phrase heard in an utterance into a tool call.
type Trigger struct {
	Phrase    string         `json:"phrase"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
	// Expect, when set, is "granted" or "denied": the run fails if this
	// trigger never fires or fires with another outcome.
	Expect string `json:"expect,omitempty"`
	// Note is shown in the transcript, e.g. who really said it.
	Note string `json:"note,omitempty"`
}

type event struct {
	Type       string  `json:"type"`
	Subject    *string `json:"subject"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Text       string  `json:"text"`
	Candidates []struct {
		IdentityID string  `json:"identityId"`
		LLR        float64 `json:"llr"`
	} `json:"candidates"`
}

// Call is one tool call the runtime made and what came of it.
type Call struct {
	Trigger  int
	Tool     string
	Subject  *string
	Granted  bool
	Decision *decision.Decision
	Text     string
}

type runtime struct {
	gateway  string
	http     *http.Client
	triggers []Trigger
	owner    string
	names    map[string]string
	out      io.Writer

	session string
	nextID  int
	heard   map[string]bool // subjects the producer has sent evidence for
	pending map[string][]event
	order   []string
	calls   []Call
}

func main() {
	gw := flag.String("gateway", "", "attest-gateway MCP endpoint, e.g. http://127.0.0.1:8430/mcp")
	triggersPath := flag.String("triggers", "", "triggers JSON file")
	owner := flag.String("owner", "", "the owner's identity id (for the outcome check)")
	namesJSON := flag.String("names", "{}", `display names for identity ids, JSON {"id":"name"}`)
	lenient := flag.Bool("lenient", false, "check only trigger expectations and invariants, not the car scenario's three outcomes")
	flag.Parse()
	if *gw == "" || *triggersPath == "" {
		fmt.Fprintln(os.Stderr, "usage: runtime -gateway URL -triggers FILE [-owner ID] < producer-stream.jsonl")
		os.Exit(2)
	}
	triggers, err := loadTriggers(*triggersPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	rt := &runtime{
		gateway: *gw, http: &http.Client{Timeout: 15 * time.Second}, triggers: triggers,
		owner: *owner, names: map[string]string{}, out: os.Stdout,
		heard: map[string]bool{}, pending: map[string][]event{},
	}
	if err := json.Unmarshal([]byte(*namesJSON), &rt.names); err != nil {
		fmt.Fprintln(os.Stderr, "-names:", err)
		os.Exit(2)
	}
	if err := rt.connect(); err != nil {
		fmt.Fprintln(os.Stderr, "connect to the gateway:", err)
		os.Exit(1)
	}
	if err := rt.consume(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "read stream:", err)
		os.Exit(1)
	}
	if failures := rt.check(!*lenient); len(failures) > 0 {
		fmt.Fprintln(rt.out, "\nRESULT: FAIL")
		for _, f := range failures {
			fmt.Fprintln(rt.out, "  - "+f)
		}
		os.Exit(1)
	}
	fmt.Fprintln(rt.out, "\nRESULT: PASS")
}

func loadTriggers(path string) ([]Trigger, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Triggers []Trigger `json:"triggers"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, t := range file.Triggers {
		if normalize(t.Phrase) == "" || t.Tool == "" {
			return nil, fmt.Errorf("%s: every trigger needs a phrase and a tool", path)
		}
		if t.Expect != "" && t.Expect != "granted" && t.Expect != "denied" {
			return nil, fmt.Errorf("%s: expect must be granted or denied, got %q", path, t.Expect)
		}
	}
	return file.Triggers, nil
}

var nonWord = regexp.MustCompile(`[^\p{L}\p{N}']+`)

// normalize lowercases and reduces punctuation to single spaces, so a phrase
// matches whole words regardless of ASR punctuation and case.
func normalize(s string) string {
	return strings.TrimSpace(nonWord.ReplaceAllString(strings.ToLower(s), " "))
}

func matches(text, phrase string) bool {
	return strings.Contains(" "+normalize(text)+" ", " "+normalize(phrase)+" ")
}

// consume reads the producer's stream. An utterance is acted on once the
// producer has sent evidence for its speaker: the producer reports a
// speaker's voiceprint evidence at the end of each of their turns, which can
// be after the words. Utterances nobody can be attributed to are acted on at
// once; there will never be evidence for them.
func (rt *runtime) consume(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "evidence":
			if ev.Subject == nil {
				continue
			}
			rt.evidence(ev)
			key := *ev.Subject
			rt.heard[key] = true
			for _, u := range rt.pending[key] {
				rt.utterance(u, "")
			}
			delete(rt.pending, key)
		case "utterance":
			if ev.Subject != nil && !rt.heard[*ev.Subject] {
				if _, ok := rt.pending[*ev.Subject]; !ok {
					rt.order = append(rt.order, *ev.Subject)
				}
				rt.pending[*ev.Subject] = append(rt.pending[*ev.Subject], ev)
				continue
			}
			rt.utterance(ev, "")
		}
	}
	for _, key := range rt.order {
		for _, u := range rt.pending[key] {
			rt.utterance(u, "the producer never sent evidence for this speaker")
		}
	}
	return sc.Err()
}

func (rt *runtime) name(id string) string {
	if n := rt.names[id]; n != "" {
		return n
	}
	return id
}

func (rt *runtime) evidence(ev event) {
	parts := make([]string, len(ev.Candidates))
	for i, c := range ev.Candidates {
		parts[i] = fmt.Sprintf("%s %+.1f", rt.name(c.IdentityID), c.LLR)
	}
	what := strings.Join(parts, ", ")
	if what == "" {
		what = "matched no enrolled voice"
	}
	fmt.Fprintf(rt.out, "  · voice evidence for %s: %s (LLR)\n", *ev.Subject, what)
}

func (rt *runtime) utterance(u event, note string) {
	who := "nobody (words not attributable to one speaker)"
	if u.Subject != nil {
		who = *u.Subject
	}
	fmt.Fprintf(rt.out, "\n[%6.2f–%6.2f] %s\n    %q\n", u.Start, u.End, who, u.Text)
	if note != "" {
		fmt.Fprintf(rt.out, "    (%s)\n", note)
	}
	for i, t := range rt.triggers {
		if !matches(u.Text, t.Phrase) {
			continue
		}
		fmt.Fprintf(rt.out, "    trigger %q -> %s", t.Phrase, t.Tool)
		if t.Note != "" {
			fmt.Fprintf(rt.out, "   [%s]", t.Note)
		}
		fmt.Fprintln(rt.out)
		c := rt.callTool(u.Subject, t)
		c.Trigger = i
		rt.calls = append(rt.calls, c)
		rt.report(c)
	}
}

func (rt *runtime) report(c Call) {
	verdict := "DENIED"
	if c.Granted {
		verdict = "GRANTED"
	}
	subject := "no subject sent"
	if c.Subject != nil {
		subject = "subject " + *c.Subject
	}
	identity := "identity: none"
	if c.Decision != nil && c.Decision.IdentityID != nil {
		identity = "identity: " + rt.name(*c.Decision.IdentityID)
	}
	fmt.Fprintf(rt.out, "    %s (%s, %s)\n", verdict, subject, identity)
	if c.Decision != nil {
		for _, r := range c.Decision.Reasons {
			fmt.Fprintf(rt.out, "      - %s\n", r)
		}
	}
	if c.Text != "" {
		fmt.Fprintf(rt.out, "    tool says: %s\n", c.Text)
	}
}

// rpc posts one JSON-RPC message to the gateway.
func (rt *runtime) rpc(method string, params any, notify bool) (*http.Response, map[string]any, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if !notify {
		rt.nextID++
		msg["id"] = rt.nextID
	}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, rt.gateway, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if rt.session != "" {
		req.Header.Set("Mcp-Session-Id", rt.session)
	}
	resp, err := rt.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if notify {
		return resp, nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return resp, nil, fmt.Errorf("status %d, unreadable reply: %.200s", resp.StatusCode, raw)
	}
	if e, ok := out["error"].(map[string]any); ok {
		return resp, out, fmt.Errorf("JSON-RPC error: %v", e["message"])
	}
	return resp, out, nil
}

// connect runs the MCP handshake through the gateway; only tools/call is
// gated, so this passes straight through to the MCP server.
func (rt *runtime) connect() error {
	resp, out, err := rt.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "car-demo-runtime", "version": "0.1.0"},
	}, false)
	if err != nil {
		return err
	}
	rt.session = resp.Header.Get("Mcp-Session-Id")
	info, _ := out["result"].(map[string]any)["serverInfo"].(map[string]any)
	if _, _, err := rt.rpc("notifications/initialized", nil, true); err != nil {
		return err
	}
	_, out, err = rt.rpc("tools/list", nil, false)
	if err != nil {
		return err
	}
	var names []string
	list, _ := out["result"].(map[string]any)["tools"].([]any)
	for _, t := range list {
		if m, ok := t.(map[string]any); ok {
			names = append(names, fmt.Sprint(m["name"]))
		}
	}
	fmt.Fprintf(rt.out, "MCP server %v via the gateway; tools: %s\n", info["name"], strings.Join(names, ", "))
	return nil
}

func (rt *runtime) callTool(subject *string, t Trigger) Call {
	c := Call{Tool: t.Tool, Subject: subject}
	params := map[string]any{"name": t.Tool, "arguments": t.Arguments}
	if t.Arguments == nil {
		params["arguments"] = map[string]any{}
	}
	// The speaker's subject key, from the producer's attribution. Omitted when
	// the words belong to nobody: attest then grants floor permissions only.
	if subject != nil {
		params["_meta"] = map[string]any{"attest/subject": *subject}
	}
	resp, out, err := rt.rpc("tools/call", params, false)
	if err != nil {
		c.Text = "call failed: " + err.Error()
		return c
	}
	result, _ := out["result"].(map[string]any)
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		if m, ok := content[0].(map[string]any); ok {
			c.Text, _ = m["text"].(string)
		}
	}
	if h := resp.Header.Get(gateway.DecisionHeader); h != "" {
		// A forwarded call: the gateway reports the grant in a header.
		var d decision.Decision
		if json.Unmarshal([]byte(h), &d) == nil {
			c.Decision = &d
		}
	} else if meta, ok := result["_meta"].(map[string]any); ok {
		// A refusal carries the decision in the tool result.
		raw, _ := json.Marshal(meta["attest/decision"])
		var d decision.Decision
		if json.Unmarshal(raw, &d) == nil && len(d.Reasons) > 0 {
			c.Decision = &d
		}
	}
	isError, _ := result["isError"].(bool)
	c.Granted = !isError && c.Decision != nil && c.Decision.Granted
	return c
}

// check returns why the run failed, if it did.
func (rt *runtime) check(scenario bool) []string {
	var failures []string
	verdict := func(granted bool) string {
		if granted {
			return "granted"
		}
		return "denied"
	}
	for i, t := range rt.triggers {
		if t.Expect == "" {
			continue
		}
		fired := false
		for _, c := range rt.calls {
			if c.Trigger != i {
				continue
			}
			fired = true
			if verdict(c.Granted) != t.Expect {
				failures = append(failures, fmt.Sprintf("trigger %q: %s, expected %s", t.Phrase, verdict(c.Granted), t.Expect))
			}
		}
		if !fired {
			failures = append(failures, fmt.Sprintf("trigger %q (expected %s) never fired: the phrase was not heard", t.Phrase, t.Expect))
		}
	}
	isOwner := func(c Call) bool {
		return rt.owner != "" && c.Decision != nil && c.Decision.IdentityID != nil && *c.Decision.IdentityID == rt.owner
	}
	for _, c := range rt.calls {
		if c.Granted && c.Subject == nil {
			failures = append(failures, c.Tool+" was granted with no subject")
		}
		if c.Granted && rt.owner != "" && !isOwner(c) {
			failures = append(failures, c.Tool+" was granted for someone other than the owner")
		}
	}
	if !scenario {
		return failures
	}
	outcomes := []struct {
		want string
		ok   func(Call) bool
	}{
		{"the owner's calendar_today call is granted", func(c Call) bool {
			return c.Tool == "calendar_today" && c.Granted && isOwner(c)
		}},
		// Denials must come from attest (with reasons), not from an outage.
		{"the passenger's email_send call is denied", func(c Call) bool {
			return c.Tool == "email_send" && c.Subject != nil && !c.Granted && c.Decision != nil && !isOwner(c)
		}},
		{"a call with no subject is denied", func(c Call) bool {
			return c.Subject == nil && !c.Granted && c.Decision != nil
		}},
	}
	fmt.Fprintln(rt.out, "\nOutcome:")
	for _, o := range outcomes {
		ok := slices.ContainsFunc(rt.calls, o.ok)
		fmt.Fprintf(rt.out, "  [%s] %s\n", map[bool]string{true: "x", false: " "}[ok], o.want)
		if !ok {
			failures = append(failures, "not seen: "+o.want)
		}
	}
	return failures
}
