package ingest

import "strings"

// Route describes a parsed MQTT topic.
type Route struct {
	Handler string // handler name: "status", "console", "systems", "system", "calls_active", "call_start", "call_end", "audio", "recorders", "recorder", "rates", "trunking_message", "unit_event" (includes signal)
	SysName string // set for unit event and trunking message topics
}

// ParseTopic maps an MQTT topic string to a Route.
//
// Routing is based entirely on the trailing segments of the topic — the prefix
// is ignored. This means any topic prefix configured in the TR MQTT plugin works
// as long as MQTT_TOPICS is set to match.
//
// Feed topics ({topic}/...):
//
//	.../trunk_recorder/status  → status
//	.../trunk_recorder/console → console
//	.../systems                → systems
//	.../system                 → system
//	.../calls_active           → calls_active
//	.../call_start             → call_start
//	.../call_end               → call_end
//	.../audio                  → audio
//	.../recorders              → recorders
//	.../recorder               → recorder
//	.../rates                  → rates
//	.../config                 → config
//
// Trunking message topics ({message_topic}/...):
//
//	.../{sys_name}/message → trunking_message
//
// Unit event topics ({unit_topic}/...):
//
//	.../{sys_name}/{event_type} → unit_event
func ParseTopic(topic string) *Route {
	parts := strings.Split(topic, "/")
	n := len(parts)
	if n < 2 {
		return nil
	}

	last := parts[n-1]

	// Two-segment matches: trunk_recorder/status, trunk_recorder/console
	if n >= 2 && parts[n-2] == "trunk_recorder" {
		switch last {
		case "status":
			return &Route{Handler: "status"}
		case "console":
			return &Route{Handler: "console"}
		}
	}

	// Single-segment feed handlers
	switch last {
	case "systems", "system", "calls_active", "call_start", "call_end",
		"audio", "recorders", "recorder", "rates", "config":
		return &Route{Handler: last}
	}

	// Trunking messages: .../{sys_name}/message
	if last == "message" && n >= 2 {
		return &Route{Handler: "trunking_message", SysName: parts[n-2]}
	}

	// DVCF file messages: .../dvcf
	if last == "dvcf" {
		return &Route{Handler: "dvcf"}
	}

	// Unit events: .../{sys_name}/{event_type}
	switch last {
	case "on", "off", "call", "end", "join", "location", "ackresp", "data", "signal":
		if n >= 2 {
			return &Route{Handler: "unit_event", SysName: parts[n-2]}
		}
	}

	return nil
}
