-- phase: expand
-- Announced maintenance now does what it announces (SPEC 17.9, managed upgrades): when its time comes the tenant's writes are held (a pause on the
-- organization), and when it ends, or is cancelled, they are released. The columns record that, once, whichever node got there first.
ALTER TABLE maintenance_notice ADD COLUMN hold_writes boolean NOT NULL DEFAULT true;
ALTER TABLE maintenance_notice ADD COLUMN started_at timestamptz;
ALTER TABLE maintenance_notice ADD COLUMN finished_at timestamptz;
ALTER TABLE maintenance_notice ADD COLUMN last_error text NOT NULL DEFAULT '';
