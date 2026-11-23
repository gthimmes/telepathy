package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gthimmes/telepathy/internal/query"
	"github.com/gthimmes/telepathy/internal/store"
)

const pagerDutyEvents = "https://events.pagerduty.com/v2/enqueue"

type notifier struct {
	Client       *http.Client
	PagerDutyURL string // override for tests
}

func (e *Engine) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// Link returns the URL that opens an alert's scope in Explain.
func (e *Engine) Link(a *store.Alert) string {
	base := strings.TrimRight(e.PublicURL, "/")
	var d Details
	json.Unmarshal(a.Details, &d)
	q := url.Values{"filter": {d.Scope}, "tab": {"explain"}}
	return base + "/#/explore?" + q.Encode()
}

// notify sends an alert event to every channel whose routing matches the
// alert, unless a silence covers it, and returns the last error text.
func (e *Engine) notify(event string, a *store.Alert) string {
	nowMs := time.Now().UnixMilli()
	if silences, err := e.Store.ListSilences(nowMs); err == nil {
		for _, s := range silences {
			if !s.Covers(a.Service, a.Location, nowMs) {
				continue
			}
			var d Details
			json.Unmarshal(a.Details, &d)
			d.Silenced = s.Reason
			if d.Silenced == "" {
				d.Silenced = "silenced"
			}
			a.Details, _ = json.Marshal(d)
			e.Store.SaveAlert(a)
			e.logf("alerting: %s (%s) not notified: silenced", a.Title, event)
			return ""
		}
	}
	chans, err := e.Store.ListChannels()
	if err != nil {
		return err.Error()
	}
	last := ""
	for _, c := range chans {
		if !c.Routes(a.Service, a.Location, a.Severity) {
			continue
		}
		if err := e.Send(c, event, a); err != nil {
			last = c.Name + ": " + err.Error()
			e.logf("alerting: notify %s failed: %v", c.Name, err)
		}
	}
	return last
}

// Send delivers one alert event to one channel, retrying once.
func (e *Engine) Send(c store.Channel, event string, a *store.Alert) error {
	target, body, err := e.payload(c, event, a)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = e.post(target, body)
		if err == nil || attempt == 1 {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (e *Engine) post(target string, body []byte) error {
	resp, err := e.client().Post(target, "application/json", bytes.NewReader(body))
	if err != nil {
		// Never echo the URL: webhook URLs are secrets.
		if ue, ok := err.(*url.Error); ok {
			return fmt.Errorf("request failed: %v", ue.Err)
		}
		return fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint answered %s", resp.Status)
	}
	return nil
}

func (e *Engine) payload(c store.Channel, event string, a *store.Alert) (target string, body []byte, err error) {
	var d Details
	json.Unmarshal(a.Details, &d)
	link := e.Link(a)
	switch c.Kind {
	case "slack":
		body, err = json.Marshal(map[string]string{"text": slackText(event, a, d, link)})
		return c.URL, body, err
	case "pagerduty":
		action := "trigger"
		if event == "resolved" {
			action = "resolve"
		}
		sev := "critical"
		if a.Severity != query.Critical {
			sev = "warning"
		}
		target = e.PagerDutyURL
		if target == "" {
			target = pagerDutyEvents
		}
		body, err = json.Marshal(map[string]any{
			"routing_key": c.URL, "event_action": action, "dedup_key": fmt.Sprintf("telepathy-%d", a.ID),
			"payload": map[string]any{
				"summary": a.Title + ": " + a.Summary, "source": "telepathy", "severity": sev,
				"component": a.Service, "group": a.Location,
				"custom_details": map[string]any{"reasons": d.Reasons, "likely_triggers": d.Triggers, "also_affected": d.Impacted, "findings": findingLines(d)},
			},
			"links": []map[string]string{{"href": link, "text": "Open in Telepathy"}},
		})
		return target, body, err
	case "webhook":
		body, err = json.Marshal(map[string]any{"event": event, "alert": a, "url": link})
		return c.URL, body, err
	}
	return "", nil, fmt.Errorf("unknown channel kind %q", c.Kind)
}

func findingLines(d Details) []string {
	out := []string{}
	for _, f := range d.Findings {
		out = append(out, fmt.Sprintf("%s=%s (%.0f%% of affected vs %.0f%% of the rest)", f.Key, f.Value, f.SelPct, f.BasePct))
	}
	return out
}

// slackEscape neutralizes Slack's control characters in telemetry-derived text.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func slackText(event string, a *store.Alert, d Details, link string) string {
	icon, verb := ":red_circle:", ""
	switch {
	case event == "resolved":
		icon, verb = ":large_green_circle:", "Resolved: "
	case event == "escalated":
		verb = "Escalated: "
	case a.Severity != query.Critical:
		icon = ":large_yellow_circle:"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s *%s%s*", icon, verb, slackEscape(a.Title))
	if event == "resolved" {
		fmt.Fprintf(&b, "\nLasted %s.", (time.Duration(a.ResolvedMs-a.StartedMs) * time.Millisecond).Round(time.Second))
	} else {
		if a.Summary != "" {
			b.WriteString("\n" + slackEscape(a.Summary))
		}
		if len(d.Triggers) > 0 {
			b.WriteString("\n*Likely trigger:* " + slackEscape(strings.Join(d.Triggers, "; ")))
		}
		if len(d.Impacted) > 0 {
			b.WriteString("\n*Also affected (downstream):* " + slackEscape(strings.Join(d.Impacted, ", ")))
		}
		if lines := findingLines(d); len(lines) > 0 {
			b.WriteString("\n*What is different:*")
			for _, l := range lines {
				b.WriteString("\n• " + slackEscape(l))
			}
		}
	}
	fmt.Fprintf(&b, "\n<%s|Open in Telepathy>", link)
	return b.String()
}
