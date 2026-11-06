-- Needs cloudsql.logical_decoding=on on the instance.

ALTER ROLE ${cdc_user} WITH REPLICATION;

CREATE PUBLICATION ${publication}
  FOR TABLES IN SCHEMA sales, inventory;
