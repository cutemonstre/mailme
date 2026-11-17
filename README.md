# mailme

Email yourself a plain-text note from a terminal or a coding agent, using the Cloudflare Email Sending API (beta). Go standard library only.

```sh
mailme "pay bills by tonight"
mailme -s "Rotate API keys" "body..."
echo "long body" | mailme -s "subject"
mailme --dry-run "x"      # print the JSON payload, send nothing
mailme --version
```

## How it works

mailme builds one JSON payload and sends it to the Cloudflare Email Sending REST endpoint. The subject is the first line of the body unless you pass `-s`. It reports an error if the API says the message bounced or was suppressed.

There is deliberately no `--to` flag. The recipient comes only from config, so an agent that runs mailme cannot be talked into emailing anyone else.

## Install

```sh
go install github.com/cutemonstre/mailme@latest
```

## Config

Put `KEY=VALUE` lines in `~/.config/mailme/env` (`#` comments and quotes allowed; run `chmod 600` on it). Real environment variables override the file.

| Key | Notes |
|---|---|
| `CF_ACCOUNT_ID` | required |
| `CF_EMAIL_TOKEN` | required (not needed for `--dry-run`) |
| `MAILME_TO` | required: your own address |
| `MAILME_FROM` | required: a sender address on a domain set up for Cloudflare Email Sending, such as `me@example.com` |
| `MAILME_FROM_NAME` | default `Note to self` |
| `MAILME_API_BASE` | default `https://api.cloudflare.com/client/v4` (used by tests) |

## Test

```sh
go build ./... && go vet ./... && go test ./...
```

Built by directing AI coding agents (Claude Code), with design, review and testing by the author.

## License

MIT
