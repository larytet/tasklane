-- phase: expand
-- Email to and from customers (SPEC 15.1): a message to a customer names the customer it is for, so the send path can check that they still
-- see the request; a reply from a customer is kept with the customer it came from and becomes their comment.
ALTER TABLE mail_message ADD COLUMN customer_id uuid;
ALTER TABLE mail_inbound ADD COLUMN customer_id uuid;
ALTER TABLE customer_comment ADD COLUMN mail_inbound_id bigint;
