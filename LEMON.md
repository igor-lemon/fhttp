# fhttp for tls-forge

A fork of [bogdanfinn/fhttp](https://github.com/bogdanfinn/fhttp) v0.6.9
(`a8b14170ef54dc2355c68ec20951a86750fb7732`), on the branch `tls-forge`, for
[tls-forge](https://github.com/Sec-CH-Lemon/tls-forge).

- The module is `github.com/igor-lemon/fhttp`: every import of
  `github.com/bogdanfinn/fhttp` is renamed, so that tls-forge can require it
  without a `replace`, which `go install` refuses and a library's users never
  see. On another base:

      grep -rl github.com/bogdanfinn/fhttp --include='*.go' --include=go.mod . |
        xargs perl -pi -e 's#github\.com/bogdanfinn/fhttp#github.com/igor-lemon/fhttp#g'

  then `git diff --name-only -- '*.go' | xargs gofmt -w`, which puts the
  renamed imports back in order.

- utls is `github.com/igor-lemon/utls`, tls-forge's fork of
  bogdanfinn/utls (its `LEMON.md` says what it changes): every import of
  `github.com/bogdanfinn/utls` is renamed the same way.
- `LICENSE` is Go's: fhttp is a fork of the Go standard library's net/http,
  and ships none.
- At the base, upstream as here, 32 tests of the package itself fail
  (`TestRequestWrite`, `TestTransportGzip`, the `TestH12_*` tests and
  others), 3 of `httputil` and 2 of `http2` (`TestTransportRejectsConnHeaders`
  and `TestServer_MaxQueuedControlFrames`), and `TestOmitHTTP2` runs past ten
  minutes; the patches change none of that. Run the package with
  `-skip '^TestOmitHTTP2$'` and nothing else at once: its tests use up the
  machine's ephemeral ports, and a run straight after another fails more.

## Patches

- **How each header field is written into HPACK** (`hpack.Representation`,
  `Encoder.SetRepresentation`, `http2.Transport.HeaderRepresentation`): for a
  field not found whole in a table, whether it goes into the dynamic table,
  and whether a static name reference names the first entry with its name or
  the last. Unset, every field is written as before. Chrome writes `:path`
  without indexing, named by the first static `:path` (index 4); x/net's
  encoder indexes it and names it by the last (5). Tests in
  `http2/hpack/representation_test.go` and `http2/representation_test.go`.
- **A field too large for the HPACK table, indexed all the same**
  (`hpack.Representation.IndexTooLarge`): a field larger than the dynamic
  table is written with incremental indexing, which empties the table (RFC
  7541, section 4.4), rather than without indexing, which leaves it as it
  was. Chrome's encoder indexes every field its policy says to, whatever its
  size: a cookie of 4 KiB empties the table, and the requests after it are
  written whole. Tests in `http2/hpack/representation_test.go`.
- **The encoder's table as large as the peer allows**
  (`http2.Transport.MaxEncoderHeaderTableSize`, x/net's name and meaning):
  a peer's SETTINGS_HEADER_TABLE_SIZE is taken up to it, 4096 when it is
  unset, as before. Chrome takes whatever the peer allows, and says so with a
  Dynamic Table Size Update at the start of the next header block. Tests in
  `http2/tablesize_test.go`.
- **A HEADERS priority per request** (`http2.WithHeaderPriority`): a request
  whose context carries one sends it on its HEADERS frame, in place of the
  Transport's HeaderPriority. Chrome gives a page weight 256 and a script's
  request 220 on one connection, which a priority per transport made
  tls-forge send on connections of their own. Tests in
  `http2/priority_test.go`.
- **A connection shared with another host** (`http2.Transport.Coalesce`,
  and `RoundTripOpt.OnlyCachedConn` honoured): when the default pool holds no
  connection for a request's host:port that can take it, it asks Coalesce,
  with the connections it holds that can, in the order they were made,
  whether the request may go out on one of them instead of on one dialed for
  it (RFC 9113, section 9.1.1); the one named carries that host:port's
  requests from then on, while it can take them. A request that asks for its
  connection to be closed is never shared one. With OnlyCachedConn, the
  request goes out only on a connection the pool holds or shares, and
  ErrNoCachedConn says there is none; tried again after one, it may dial.
  Unset, every connection is dialed for its host:port, as before. Chrome
  sends a request to another name over an HTTP/2 connection it holds at an
  address the name has, under a certificate good for it. Tests in
  `http2/coalesce_test.go`.
- **A request that shares no connection** (`http2.WithCoalescing`): a
  request whose context says it may not goes out only on a connection dialed
  for its own host:port, one dialed for it when the default pool holds none
  that can take it, and Coalesce is not asked; and a connection dialed for a
  host:port carries its requests from then on, in place of those shared with
  it. Chrome sends a request a server answered 421 (Misdirected Request)
  again with IP-based pooling off, so on a connection of its host's own, and
  the host's later requests go out there (Chromium's HttpNetworkTransaction
  and SpdySessionPool::CreateSession; measured by tls-forge). Tests in
  `http2/coalesce_test.go`.
- **The cookies of an answer kept as a request says**
  (`WithResponseCookies`): a Client asks the function a request's context
  carries, of each answer to it, whether its Jar is given the cookies the
  answer sets, and gives none when it says no; without one, every answer's,
  as before. Chrome's network stack never hands over a 421 (Misdirected
  Request) it sends the request again for, and keeps nothing it sets
  (measured by tls-forge). Tests in `responsecookies_test.go`.
- **WINDOW_UPDATE as Chrome sends them** (`http2.Transport.WindowUpdateDelay`):
  when set, the transport tells the peer of the window its reader frees as
  Chrome's network stack does (SpdySession and
  SpdyStream::IncreaseRecvWindowSize; measured on Chrome 155 by tls-forge):
  what is freed and untold of a window, once it is more than half of that
  window, the connection's (the RFC's 65535 and ConnectionFlow) or a stream's
  (the SETTINGS_INITIAL_WINDOW_SIZE it advertised, whatever the peer's own),
  or at the first byte freed more than the delay after the peer was last
  told, or after the connection or stream began, however little; the
  connection's first; nothing more for a stream the peer ended or that was
  reset. Padding, data for a stream that is gone and what a body let go of
  held unread are freed at once and told of by the same rule. A body let go
  of and a frame put into it are one under the connection's lock, so that a
  frame is unread in the body or comes after the reset, and is never lost to
  the window, as the transport's own rule loses one between them. Chrome
  waits five seconds. Unset, the transport's own rule, as before: a stream
  told of every 16 KiB read while the peer's own window is under 1 MiB, and
  the connection given back up to ConnectionFlow once less than half of that
  is left. Tests in `http2/windowupdate_test.go`.
- **The header a request is written with, per protocol**
  (`Request.HeaderFor`, `Request.WrittenHeader`, `ProtocolHTTP1`,
  `ProtocolHTTP2`): once the transport knows the protocol a request goes
  over, it asks the request for the header to write, given the Header as the
  client left it; nil is the Header. The HTTP/1.1 round trip and `Write`,
  and the HTTP/2 round trip, check, write and decide on what it gives, and
  answer with the request they were given. A redirect's request, made anew,
  has none. Chrome writes one request differently over HTTP/1.1 and HTTP/2,
  and ALPN, encrypted in TLS 1.3, says which only after the handshake;
  tls-forge had changed the header in place from the trace's GotConn. Tests
  in `headerfor_test.go` and `http2/headerfor_test.go`.
- **A header block cut into frames no larger than allowed**
  (`http2.Transport.MaxHeaderFramePayload`, and room for the priority): a
  HEADERS frame's fragment leaves room for its five bytes of priority, which
  a full fragment and the priority took past the peer's
  SETTINGS_MAX_FRAME_SIZE, a frame the peer refuses with FRAME_SIZE_ERROR;
  and with MaxHeaderFramePayload set below the peer's, every HEADERS and
  CONTINUATION frame carries at most that much, priority included. Chrome
  writes at most 16,374 bytes a frame, whatever the peer allows. Unset, a
  frame is as large as the peer allows, as before. Tests in
  `http2/headerframes_test.go`.
- **A header block of more than one frame in one write**: its HEADERS and
  CONTINUATION frames go to the connection at once, so that over TLS a record
  carries the end of one frame and the start of the next, as Chrome's do (an
  X-Foo of 30,000 letters: a record of 16,384 bytes and one of the rest).
  Through the connection's buffer of 4 KiB the HEADERS frame, larger than the
  buffer, went past it on its own, and the CONTINUATION after it: a record
  each. A block of one frame is written as before. Tests in
  `http2/headerframes_test.go` (`writeLog`). Equivalent mutant: a block of
  one frame taken the same way, which writes the same bytes in one write.
- **A request written over HTTP/1.1 as Chrome writes it**
  (`http.Transport.MaxMergedHeadAndBody`): with it set, a request's head,
  the request line and the header lines, is put together whole and given to
  the connection in one write, whatever its size, and a body held in memory
  (`heldInMemory`: a bytes.Reader, bytes.Buffer or strings.Reader, as it is
  or in an io.NopCloser or a readTrackingBody) of a known length goes in the
  same write when the two are no more than that many bytes together; any
  other body after the head, as before. Chrome's HttpStreamParser merges up
  to 1,400 bytes (kMaxMergedHeaderAndBodySize) and writes a head at once,
  where the connection's buffer of 4 KiB cut a larger head into pieces of
  its size. Unset, a request is written as before: `isKnownInMemoryReader`
  is left as it was, though it misses the io.NopCloser of every in-memory
  reader since Go 1.20 (nopCloserWriterTo), so the head is flushed before
  every body there. Tests in `headwrites_test.go` and
  `headwrites_internal_test.go`; `TestTransportClone` sets the field.
  Equivalent mutants: the new path's `FlushHeaders` (set already for every
  body), the error of a head's write (bufio's sticky error fails the flush
  the same way), and `closed` where the body is merged (an in-memory body's
  Close does nothing).
- **ALPS taken as Chrome takes it** (`http2/alps.go`): what a server gave
  in ALPS for h2 (the TLS state's `PeerApplicationSettings`) is read before
  the connection's first request as Chrome 155 was measured reading it
  (tls-forge's bench, 73 scenarios, measured twice): HTTP/2 frames, each held
  to what HTTP/2 says a frame of its type may be (on a stream or on none, of
  its length, within 16 KiB, a CONTINUATION where a header block goes on and
  only there, its padding within it, its header block one HPACK decodes, one
  decoder for all of ALPS); SETTINGS taken, unacknowledged, as Chromium's
  SpdySession::HandleSetting takes them (the header table, the streams at a
  time, a window up to 2^31-1 and one past it passed over;
  ENABLE_CONNECT_PROTOCOL and NO_RFC7540_PRIORITIES other than 0 or 1
  refused, whatever was said before them (1 then 0 of either is taken);
  MAX_FRAME_SIZE, MAX_HEADER_LIST_SIZE and ENABLE_PUSH passed over:
  `setInitialWindowSize` and `setHeaderTableSize` are taken out of
  `applySetting`, which the SETTINGS frame keeps); a frame of a type HTTP/2
  does not have passed over, ALTSVC, ORIGIN, PRIORITY_UPDATE and ACCEPT_CH's
  entries among them, but an ACCEPT_CH frame on a stream or with flags
  refused. ALPS Chrome refuses ends the connection after the preface and
  SETTINGS with GOAWAY PROTOCOL_ERROR saying `Error parsing ALPS: N`
  (Chromium's AlpsDecoder::Error: 1 a frame that does not read, at once;
  past the rest 2 a frame ALPS may not carry, then 4 a SETTINGS
  acknowledgement, then 3 a frame cut short, then 5 an ACCEPT_CH frame on a
  stream or 6 with flags, the first of these), or Chrome's own words for a
  setting it refuses; and the request fails, tried on no other connection:
  the second connection Chrome was measured making is its error page,
  reloading itself a second later. Tests in `http2/alps_test.go`; 62
  mutants, all killed.
- **A body sent in DATA frames no larger than allowed**
  (`http2.Transport.MaxDataFramePayload`): with it set, a request's body is
  read and sent that much at a time, each read a DATA frame, the last ending
  the stream, as the windows allow. Chrome writes at most 16,375 bytes a
  frame, which with its header fills a TLS record of 16,384, whatever the peer
  allows. Unset, a frame is as large as the peer allows, as before. Tests in
  `http2/dataframes_test.go`. Equivalent mutant: reading more than the peer
  allows a frame (`readSize = limit` without the `min`), since the window's
  take is cut to the peer's frame size.
- **A dial that ends with its request's context**
  (`http2.Transport.DialTLSContext`, x/net's name and meaning): a connection
  is dialed with the context of the request it is dialed for, which ends the
  dial and its handshake; DialTLS, which takes none, is used without it, and
  the default dial takes it too. A request waiting for a connection another
  request's dial is opening waits no longer than its own context lets it,
  and dials for itself when the other request's context ended that dial
  (x/net's shouldRetryDial); a dial that failed of itself fails every
  request waiting for it, as before. The pool takes the connection and
  forgets the dial before it says the dial is done, so a request that dials
  again starts a dial of its own. Before, the pool dialed with no context,
  and a request waited for a dial however long it took: a server that took
  the TCP connection and never answered the handshake held the request, and
  every other one to it, past their deadlines and for good. Tests in
  `http2/dialcontext_test.go`; 15 mutants, all killed but one equivalent
  (the dial said done before it is forgotten: a request that dials again then
  finds the finished dial, and goes round until it is forgotten).
