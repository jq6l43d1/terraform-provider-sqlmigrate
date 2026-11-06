-- Flyway runs each versioned file once and records it in flyway_schema_history.

CREATE SCHEMA IF NOT EXISTS sales;
CREATE SCHEMA IF NOT EXISTS inventory;

-- The login itself is created by Terraform (google_sql_user); this grants it
-- what Datastream needs to read.
GRANT USAGE ON SCHEMA sales, inventory TO ${cdc_user};
GRANT SELECT ON ALL TABLES IN SCHEMA sales, inventory TO ${cdc_user};
ALTER DEFAULT PRIVILEGES IN SCHEMA sales, inventory
  GRANT SELECT ON TABLES TO ${cdc_user};
