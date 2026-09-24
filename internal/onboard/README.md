# internal/onboard

A new team's first configuration: what `hearsay init` writes. It takes what a
person answered (`Input`), reads who is on the team from the sources it has
credentials for (`Directory`), and produces the single-file `hearsay.yaml` and
the env file holding the secrets that file names (`Generate`), which `Write`
validates with `config.Load` and puts on disk.

**Belongs here:** turning answers and source reads into configuration text,
the rules for which identities are written on whom, and writing the two files
safely.

**Does not belong here:** prompts and flags, which are `cmd/hearsay`'s; the
HTTP calls, which are the connector packages' (`github.Reader.Collaborators`,
`discord.Connector.Member`, `drive.Connector.FolderUsers`); and what a valid
configuration is, which is `internal/config`'s alone. Nothing here checks what
the loader checks: `Write` loads the generated file and refuses to write one
that does not load.

## Things to know before changing it

- **No secret it did not make.** The env file holds the API tokens generated
  here and an empty entry for every other variable the configuration names.
  The credentials init reads the sources with come from the environment and
  are never written, not even into the env file.
- **Identities are confirmed, not guessed.** Principals come from GitHub's
  collaborators, with login and node id. A Discord account is written on one
  only when their GitHub profile links it and the guild has that member; a
  Drive account only when their public GitHub email is an address a folder is
  shared with. A Slack account is added only when its email uniquely matches
  one seeded principal. An account two people claim is written on neither. A handle
  that merely looks the same in two sources is not evidence
  (docs/config.md, "Matching is per source").
- **The operator's word is taken as given.** What the person running init says
  about their own accounts is written as they said it; the sources only add
  the native id or handle they confirm.
- **Written defaults are the loader's.** The authority policy and the model
  tiers are rendered from `config.DefaultPolicy` and `llm.Default`, so the file
  says what applies rather than a copy that can drift; the command's tests load
  the file with and without those sections and compare.
- **Nothing is overwritten by accident.** Without `overwrite`, `Write` refuses
  if either file exists and writes neither; each file is created exclusively.
  With it, each file is written beside its target and renamed over it.
