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

- `LICENSE` is Go's: fhttp is a fork of the Go standard library's net/http,
  and ships none.
- At the base `TestTransportRejectsConnHeaders` in `http2` fails, upstream as
  here.
