-- Seed data for the local Redash sandbox; scripts/redash-up.sh registers the
-- database this creates as a Redash data source. The column types cover what
-- `rdsh query create --param-type` can express, so a parametrized query has a
-- date, a number and a text column to filter on.
--
-- This runs from the postgres image's /docker-entrypoint-initdb.d, which feeds
-- the file to psql — hence \connect — and only on the first boot of the data
-- volume, so `redash-up.sh --reset` is what replays it.
CREATE DATABASE testdata;
\connect testdata

CREATE TABLE signups (
    id           integer PRIMARY KEY,
    team         text    NOT NULL,
    plan         text    NOT NULL,
    signed_up_on date    NOT NULL,
    seats        integer NOT NULL
);

-- 40 rows spanning 2025-11-04 to 2026-03-01, so that a `signed_up_on >=
-- '2026-01-01'` filter returns a proper subset rather than every row.
INSERT INTO signups (id, team, plan, signed_up_on, seats)
SELECT g,
       (ARRAY['core', 'growth', 'platform'])[1 + g % 3],
       (ARRAY['free', 'team', 'enterprise'])[1 + g % 3],
       DATE '2025-11-01' + g * 3,
       1 + g % 12
FROM generate_series(1, 40) AS g;

CREATE TABLE events (
    id          integer        PRIMARY KEY,
    signup_id   integer        NOT NULL REFERENCES signups (id),
    kind        text           NOT NULL,
    occurred_at timestamptz    NOT NULL,
    amount      numeric(10, 2) NOT NULL
);

INSERT INTO events (id, signup_id, kind, occurred_at, amount)
SELECT g,
       1 + g % 40,
       (ARRAY['invoice', 'refund', 'credit'])[1 + g % 3],
       TIMESTAMPTZ '2025-11-01 09:00:00+00' + g * INTERVAL '17 hours',
       (10 + g % 90)::numeric + 0.50
FROM generate_series(1, 120) AS g;
