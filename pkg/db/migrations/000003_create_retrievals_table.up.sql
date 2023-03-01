BEGIN;

CREATE TABLE retrievals
(
    id         INT GENERATED ALWAYS AS IDENTITY,
    node_id    INT         NOT NULL,
    duration   FLOAT,
    error      TEXT,
    created_at TIMESTAMPTZ NOT NULL,

    CONSTRAINT fk_retrievals_node
        FOREIGN KEY (node_id)
            REFERENCES nodes (id),

    PRIMARY KEY (id)
);

CREATE INDEX idx_retrievals_created_at ON retrievals (created_at);

COMMIT;

