-- +goose Up

-- What the assertion worker did with each GitHub `/hearsay` comment command,
-- and the one comment it answered it with (ADR-0022, issue #182). A command
-- runs once: its row is written in the transaction that records its gesture or
-- its topic operation, or its refusal, under the scope's serial key, so a
-- retried job finds it and only posts the reply it still owes. The reply is
-- found again on GitHub by the marker it carries before one is posted, so a
-- run that died between posting and recording does not post a second.

CREATE TABLE github_command_replies (
    -- The L0 `command` event that ran. One command per comment: an edit is a
    -- later revision of the same artifact and runs nothing.
    event text PRIMARY KEY CHECK (event LIKE 'evt:%'),
    source text NOT NULL CHECK (source <> ''),
    artifact text NOT NULL CHECK (artifact <> ''),
    -- The serial key it ran under, which a job that still owes its reply is
    -- enqueued under again at startup.
    scope text NOT NULL CHECK (scope <> ''),
    -- Where the reply goes: the issue or pull request the command was written
    -- on, the command comment's id, which the reply names, and when it was
    -- written, which bounds the search for a reply already posted.
    repository text NOT NULL CHECK (repository <> ''),
    issue integer NOT NULL CHECK (issue > 0),
    comment bigint NOT NULL CHECK (comment > 0),
    commented_at timestamptz NOT NULL,
    -- The principal the commenter mapped to, where one did.
    principal text,
    -- What it recorded, if anything: a gesture keyed by `event`, or a topic
    -- operation. A refusal records neither.
    gesture bigint REFERENCES l2_gestures (id),
    operation bigint REFERENCES l2_topic_operations (id),
    -- The reply's text, without its marker. It holds ids and Hearsay's own
    -- words, never text a person wrote.
    body text NOT NULL CHECK (body <> ''),
    -- The reply comment, once it is posted or found.
    reply bigint,
    -- The tombstone of the command comment, once its deletion has been
    -- applied; what the reply says about it; and whether the reply says it yet.
    undo_event text CHECK (undo_event LIKE 'evt:%'),
    undo_body text,
    undo_revised boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT github_command_replies_one_record CHECK (gesture IS NULL OR operation IS NULL),
    CONSTRAINT github_command_replies_undo CHECK ((undo_event IS NULL) = (undo_body IS NULL))
);

CREATE UNIQUE INDEX github_command_replies_artifact_idx ON github_command_replies (source, artifact);
-- What startup looks for: a reply not posted, or not yet revised to say the
-- command was undone.
CREATE INDEX github_command_replies_owed_idx ON github_command_replies (event)
    WHERE reply IS NULL OR (undo_event IS NOT NULL AND NOT undo_revised);

-- +goose Down

-- Forgets which commands ran and which replies were posted: a replayed command
-- would run again, though the reply it already has on GitHub is found by its
-- marker. `hearsay migrate down` refuses without --i-know (ADR-0006).
DROP TABLE github_command_replies;
