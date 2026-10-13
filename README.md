# CLIProxyAPI Quota Balancer

A CLIProxyAPI scheduler plugin that spreads Claude traffic evenly across OAuth
accounts and never routes to an account at or above its usage ceiling on
either Anthropic window: the 5-hour session window or the weekly window.

Forked from [Smarty-Pants-Inc/cpa-plugin-quota-router](https://github.com/Smarty-Pants-Inc/cpa-plugin-quota-router)
(MIT), which blocked one weekly cutoff for an explicit model list. This fork
reads both windows, routes every Claude model by default, balances by
headroom, keeps conversations on one account for the prompt cache, and takes
per-account ceilings.

## How it picks

Each account's **headroom** is the smaller of its two windows' points left
under the ceiling:

    headroom = min(5h ceiling - 5h used, weekly ceiling - weekly used)

An account with 50% weekly left scores 40 at a 90% ceiling; one with 80% left
scores 70 and takes the traffic. Feeding the account with the most room pulls
every account toward the same remaining usage.

- **Ceilings are hard.** An account at or above either ceiling, refused
  upstream, or without a reading newer than `max-staleness` takes no request.
  When no account qualifies the request fails with a message naming each
  account's reason, rather than going over.
- **New conversations** go to the account with the most headroom. Accounts
  within `spread-band` points of the best count as equal, and the one carrying
  the fewest live conversations, then the one picked longest ago, takes it.
- **Conversations stick** to their account (keyed on `X-Claude-Code-Session-Id`
  or the other session headers the host recognises, per model) so the prompt
  cache stays warm. A conversation moves on its next request once its account
  reaches a ceiling, or once another account has `rebalance-margin` more
  points of headroom. Moving costs one cold prompt cache.
- **Readings** come from Anthropic's usage endpoint every `poll-interval`, and
  from the rate-limit headers on every proxied response in between, so the
  account carrying traffic updates continuously. A `rejected` window status or
  an upstream 429 blocks the account until a fresh read.

Overshoot is bounded, not zero: requests already in flight when an account
crosses its ceiling still complete. Leave a little room in the ceiling for that.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    quota-balancer:
      enabled: true
      models: ["*"]              # Claude model IDs routed; "*" or empty = all
      five-hour-ceiling: 90      # percent used
      weekly-ceiling: 90         # percent used
      accounts:                  # per-account ceilings, keyed by email or auth file name
        example@example.com:     # matches no account; a template to copy
          five-hour-ceiling: 90  # unset fields inherit the defaults
          weekly-ceiling: 90
      spread-band: 10            # headroom points treated as a tie
      rebalance-margin: 25       # headroom gap that moves a live conversation
      affinity-ttl: 1h           # idle time before a conversation binding expires
      poll-interval: 2m          # usage endpoint cadence (minimum 30s)
      max-staleness: 10m         # older readings make an account ineligible (>= 2x poll-interval)
      request-timeout: 10s
```

In the Management Center the `accounts` field is a JSON box. Copy the template
entry, change the email, and set the numbers:

```json
{
  "example@example.com": { "five-hour-ceiling": 90, "weekly-ceiling": 90 },
  "someone@example.com": { "five-hour-ceiling": 70, "weekly-ceiling": 80 }
}
```

Saving applies live. Field descriptions avoid quotes, apostrophes, `<`, `>`
and `&`: the host HTML-escapes plugin metadata and the panel shows the
entities literally.

Status: `GET /v0/management/plugins/quota-balancer/status` (management key)
lists each account's usage, ceilings, headroom, eligibility and live
conversations.

## Install

Build with Go 1.26 and a C toolchain:

```sh
make build            # dist/quota-balancer.so
```

Copy the library into CLIProxyAPI's plugin directory (`plugins/<goos>/<goarch>/`
under the host's working directory by default) and add the config block.
Requires CLIProxyAPI with the plugin ABI (v7.2.100+); the usage-header feed is
verified on v8.0.19.

## Tests

```sh
make test                                   # unit + ABI + end-to-end against the v7 module
CLIPROXYAPI_SRC=/path/to/CLIProxyAPI go test ./...   # end-to-end against another host checkout
```

## Security

The access token goes only to Anthropic's fixed HTTPS usage endpoint, with
redirects refused. Tokens and provider response bodies are never logged.

## License

MIT, as upstream.
