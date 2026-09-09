-- +goose Up

-- pgvector supplies the `vector` type L1's embedding column is declared with
-- (ADR-0004). It is the first migration so that every later one may assume the
-- type exists, and it is alone in this file so that a Postgres image without
-- the extension fails here, naming the one thing that is missing, rather than
-- part-way through a table.
CREATE EXTENSION IF NOT EXISTS vector;

-- +goose Down

-- There is no honest reverse. `CREATE EXTENSION IF NOT EXISTS` may have found
-- the extension already installed by whoever provisioned the database, and
-- dropping it would remove something this schema did not create — along with
-- every column of type `vector` anywhere in it. ADR-0006 says a down migration
-- that would destroy data says so and fails rather than pretending to reverse.
-- +goose StatementBegin
DO $$
BEGIN
    RAISE EXCEPTION 'refusing to drop the vector extension: it may predate this schema, and dropping it drops every vector column with it (ADR-0006)';
END
$$;
-- +goose StatementEnd
