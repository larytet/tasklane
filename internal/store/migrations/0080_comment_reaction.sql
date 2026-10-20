-- phase: expand
-- Emoji reactions on comments (SPEC 4.2): a fixed set of eight, one per person per emoji, instead of "+1" replies. A reaction
-- changes neither the issue's version nor its updated time and sends no notification; it goes with its comment (deleted with it)
-- and with its person (removed on erasure).
CREATE TABLE comment_reaction (
  comment_id uuid NOT NULL REFERENCES comment(id) ON DELETE CASCADE,
  user_id    uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  emoji      text NOT NULL CHECK (emoji IN ('👍','👎','😄','🎉','😕','❤️','🚀','👀')),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (comment_id, user_id, emoji)
);
CREATE INDEX comment_reaction_user ON comment_reaction (user_id);
