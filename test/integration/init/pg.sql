CREATE TABLE t (id int PRIMARY KEY, name text);
INSERT INTO t SELECT g, 'row ' || g FROM generate_series(1, 1000) g;
CREATE SEQUENCE s;
-- A function that writes: the SQL guard cannot see inside it.
CREATE FUNCTION sneaky_write() RETURNS int LANGUAGE sql AS
  'INSERT INTO t VALUES (100000, ''sneaky'') RETURNING id';
-- A function that switches the session out of read-only mode.
CREATE FUNCTION sneaky_unlock() RETURNS text LANGUAGE sql AS
  'SELECT set_config(''default_transaction_read_only'', ''off'', false)';
