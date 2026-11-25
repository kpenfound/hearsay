#!/bin/sh
set -eu
hearsay version
unset HEARSAY_DATABASE_URL HEARSAY_GITHUB_TOKEN HEARSAY_DISCORD_TOKEN \
  HEARSAY_DRIVE_CREDENTIALS HEARSAY_SLACK_BOT_TOKEN
mkdir -p /tmp/operator-final/config
cd /tmp/operator-final
printf 'sources: []\n' > config/hearsay.yaml
init_team() {
  hearsay init --no-input "$@" \
    --operator kyle --operator-name "Kyle Penfound" --operator-github kpenfound \
    --github-repo acme/api \
    --discord-guild 824100000000000000 --discord-channel 824100000000000001 \
    --drive-folder 1AbCdEfGhIjKlMnOpQrStUvWxYz \
    --out config/hearsay.yaml --env-file hearsay.env
}
init_team --force
chmod -R a+rX config
hearsay config validate config
su nobody -s /bin/sh -c 'hearsay config validate /tmp/operator-final/config'
test "$(stat -c %a config/hearsay.yaml)" = 644
test "$(stat -c %a hearsay.env)" = 600
before=$(sha256sum config/hearsay.yaml hearsay.env)
if init_team > /tmp/refusal.txt 2>&1; then exit 1; fi
grep 'already exists' /tmp/refusal.txt
grep 'nothing was written' /tmp/refusal.txt
test "$before" = "$(sha256sum config/hearsay.yaml hearsay.env)"
printf 'operator rehearsal: ok\n'
