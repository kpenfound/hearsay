package onboard

import (
	"bytes"
	"fmt"
)

// EnvMode is the permission the env file is written with: it holds API
// tokens, so only its owner reads it.
const EnvMode = 0o600

// renderEnv writes the env file: a generated token for every principal, then
// every source and model credential the configuration names, empty, each
// under a note saying where to get it. It returns the empty variables too.
//
// The format is KEY=value lines, which both a shell (`set -a; . ./hearsay.env`)
// and a compose file's env_file read.
func renderEnv(sources []source, tokens []pair) ([]byte, []string) {
	var b bytes.Buffer
	var empty []string
	b.WriteString("# Secrets for the Hearsay configuration, written by `hearsay init`.\n" +
		"# Keep this file out of version control; only its owner can read it.\n" +
		"# Load it with `set -a; . ./hearsay.env; set +a`, or as a compose file's env_file.\n")

	b.WriteString("\n# API tokens, generated at random: one per principal, sent as `Authorization: Bearer <token>`.\n")
	for _, t := range tokens {
		fmt.Fprintf(&b, "%s=%s\n", t.key, t.value)
	}

	blank := func(name string, lines ...string) {
		b.WriteString("\n")
		for _, l := range lines {
			fmt.Fprintf(&b, "# %s\n", l)
		}
		fmt.Fprintf(&b, "%s=\n", name)
		empty = append(empty, name)
	}
	for _, s := range sources {
		switch s.id {
		case SourceGitHub:
			blank(EnvGitHubToken,
				"GitHub: a fine-grained personal access token (https://github.com/settings/personal-access-tokens/new)",
				"or a GitHub App installation token, for the repositories in the configuration, with read access to",
				"Metadata and Contents and read and write access to Issues and Pull requests.")
			blank(EnvGitHubWebhookSecret,
				"GitHub: the secret each repository's webhook signs deliveries with. Choose a random one",
				"(`openssl rand -hex 32`) and give the same value to every webhook, under Settings > Webhooks.")
		case SourceDiscord:
			blank(EnvDiscordToken,
				"Discord: the bot token, from the application's Bot page in the Discord developer portal",
				"(https://discord.com/developers/applications). Turn on the Message Content intent there too.")
		case SourceSlack:
			blank(EnvSlackAppToken,
				"Slack: create the app from the manifest in internal/connector/slack/slack.go; under Basic Information",
				"generate an app-level token (xapp-…) with connections:write for Socket Mode.")
			blank(EnvSlackBotToken,
				"Slack: install that app, then copy its Bot User OAuth Token (xoxb-…). Invite it to each",
				"public channel. For init email matching, also grant users:read and users:read.email.")
		case SourceDrive:
			blank(EnvDriveCredentials,
				"Google Drive: a service account's JSON key, on one line inside single quotes. Create it in the",
				"Google Cloud console under IAM & Admin > Service accounts > Keys, enable the Drive API, and",
				"share every folder in the configuration with the account's address.")
		}
	}
	blank(EnvAnthropicAPIKey,
		"The model tiers: an Anthropic API key, from https://console.anthropic.com/settings/keys.")
	return b.Bytes(), empty
}
