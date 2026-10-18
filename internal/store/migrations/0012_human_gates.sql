-- phase: expand
-- human gates (SPEC 18.3): a transition can require a human actor; agents may ask a person for input
ALTER TABLE transition ADD COLUMN requires_human boolean NOT NULL DEFAULT false;
ALTER TABLE issue ADD COLUMN input_requested_at timestamptz;
ALTER TABLE issue ADD COLUMN input_requested_by uuid REFERENCES app_user(id);
ALTER TABLE issue ADD COLUMN input_question text;
-- per-agent pause and the instance-wide kill switch
ALTER TABLE app_user ADD COLUMN agent_paused boolean NOT NULL DEFAULT false;
CREATE TABLE agent_switch (
  id     int PRIMARY KEY CHECK (id = 1),
  killed boolean NOT NULL DEFAULT false,
  by_id  uuid REFERENCES app_user(id),
  at     timestamptz
);
INSERT INTO agent_switch(id) VALUES (1);
