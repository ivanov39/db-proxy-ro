CREATE TABLE t (id int PRIMARY KEY, name varchar(64));
INSERT INTO t VALUES (1, 'row 1'), (2, 'row 2'), (3, 'row 3');
SET GLOBAL log_bin_trust_function_creators = 1;
DELIMITER //
-- A function that writes: the SQL guard cannot see inside it.
CREATE FUNCTION sneaky_write() RETURNS int
BEGIN
  INSERT INTO t VALUES (100000, 'sneaky');
  RETURN 1;
END//
CREATE PROCEDURE two_results()
BEGIN
  SELECT 1 AS a;
  SELECT 2 AS b, 3 AS c;
END//
DELIMITER ;
