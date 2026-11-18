# Work update exchanges

Authored, deterministic model exchanges for the end-to-end work-source tests.
They contain the complete request and a deliberately small answer: descriptive
projects, one retry decision, and one assertion. No provider was contacted.
Normal integration runs load these files and fail on an unrecorded request.

To deliberately regenerate the exchanges after reviewing the authored answers:

```sh
dagger api call hearsay go-test-base with-directory --path /src --source . with-workdir --path /src \
  with-exec --args go,test,-count=1,-tags=integration,./internal/service/api,-run,^TestPostedWork,-record-work \
  directory --path /src/internal/service/api/testdata/work export --path ./internal/service/api/testdata/work
```

That runs the tests against the migrated Postgres the test base binds and
exports the rewritten files. Review the JSON changes as code.
The attributed and unattributed decisions differ in the source author's name;
the assertion request is the same because its text contains only distillation.
