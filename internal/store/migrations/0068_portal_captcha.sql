-- phase: expand
-- A proof-of-work check before a sign-in code is mailed (SPEC 15, abuse controls). 0 is off; otherwise the number of leading zero bits a
-- browser must find in a hash, which costs a person's browser a moment and a bot farm real work.
ALTER TABLE portal_setting ADD COLUMN captcha_bits integer NOT NULL DEFAULT 0;
