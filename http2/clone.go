package http2

import (
	http "github.com/igor-lemon/fhttp"
)

// CloneFor copies the HTTP/2 configuration into a fresh HTTP/1 owner. Function
// and PushHandler callbacks retain their caller-owned identity. Connection pools,
// caches, locks and transport-owned TLS upgrade closures are rebuilt.
func (t *Transport) CloneFor(owner *http.Transport) {
	c := &Transport{
		ChromeBehavior:             t.ChromeBehavior,
		AllowHTTP:                  t.AllowHTTP,
		InitialStreamID:            t.InitialStreamID,
		ConnectionFlow:             t.ConnectionFlow,
		ConnectionFlowSet:          t.ConnectionFlowSet,
		Coalesce:                   t.Coalesce,
		DialTLSContext:             t.DialTLSContext,
		DialTLS:                    t.DialTLS,
		DisableCompression:         t.DisableCompression,
		HeaderTableSize:            t.HeaderTableSize,
		HeaderRepresentation:       t.HeaderRepresentation,
		MaxEncoderHeaderTableSize:  t.MaxEncoderHeaderTableSize,
		WindowUpdateDelay:          t.WindowUpdateDelay,
		MaxHeaderFramePayload:      t.MaxHeaderFramePayload,
		UnusedIdle:                 t.UnusedIdle,
		MaxDataFramePayload:        t.MaxDataFramePayload,
		IdleConnTimeout:            t.IdleConnTimeout,
		InitialWindowSize:          t.InitialWindowSize,
		PingTimeout:                t.PingTimeout,
		PushHandler:                t.PushHandler,
		ReadIdleTimeout:            t.ReadIdleTimeout,
		StrictMaxConcurrentStreams: t.StrictMaxConcurrentStreams,

		Behavior:          t.Behavior.Clone(),
		Priorities:        append([]Priority(nil), t.Priorities...),
		PseudoHeaderOrder: append([]string(nil), t.PseudoHeaderOrder...),
		SettingsOrder:     append([]SettingID(nil), t.SettingsOrder...),
	}
	if t.HeaderPriority != nil {
		p := *t.HeaderPriority
		c.HeaderPriority = &p
	}
	if t.Settings != nil {
		c.Settings = make(map[SettingID]uint32, len(t.Settings))
		for k, v := range t.Settings {
			c.Settings[k] = v
		}
	}
	if t.TLSClientConfig != nil {
		c.TLSClientConfig = t.TLSClientConfig.Clone()
	}
	if _, err := configureOwnedTransport(owner, c); err != nil {
		panic("http2: fresh clone owner already has a protocol registration")
	}
	owner.H2transport = c
}
