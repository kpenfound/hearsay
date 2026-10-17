# Work update exchanges

Authored, deterministic model exchanges for the end-to-end work-source tests.
They contain the complete request and a deliberately small answer: descriptive
projects, one retry decision, and one assertion. No provider was contacted.
Normal integration runs load these files and fail on an unrecorded request.

To deliberately regenerate the exchanges after reviewing the authored answers:

```sh
go test -count=1 -tags=integration ./internal/service/api -run '^TestPostedWork' -record-work
```

The database must be disposable and migrated. Review the JSON changes as code.
The attributed and unattributed decisions differ in the source author's name;
the assertion request is the same because its text contains only distillation.
