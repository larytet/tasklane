-- phase: expand
-- Files on customer requests (SPEC 15, 17.6): an attachment is shown to customers only when it is marked so (a customer's own upload is
-- marked at once, staff mark theirs), and a customer's upload remembers who made it. The portal's rules for what may be uploaded live
-- beside its other settings and default to no uploads at all.
ALTER TABLE attachment ADD COLUMN customer_id uuid REFERENCES customer(id) ON DELETE SET NULL;
ALTER TABLE attachment ADD COLUMN customer_visible boolean NOT NULL DEFAULT false;
ALTER TABLE portal_setting ADD COLUMN attach_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE portal_setting ADD COLUMN attach_max_mb integer NOT NULL DEFAULT 10;
ALTER TABLE portal_setting ADD COLUMN attach_max_files integer NOT NULL DEFAULT 5;
ALTER TABLE portal_setting ADD COLUMN attach_classes text[] NOT NULL DEFAULT '{image,document,log}';
