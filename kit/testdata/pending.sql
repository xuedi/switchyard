-- Doctrine Migration File Generated on 2026-10-13 10:00:00

-- Version DoctrineMigrations\Version20261013100000
CREATE TABLE event_tag (id INT AUTO_INCREMENT NOT NULL, event_id INT NOT NULL, name VARCHAR(64) NOT NULL, INDEX IDX_1 (event_id), PRIMARY KEY(id)) DEFAULT CHARACTER SET utf8mb4;
ALTER TABLE event ADD summary VARCHAR(255) DEFAULT NULL, ADD price DECIMAL(10, 2) DEFAULT '0.00' NOT NULL;
ALTER TABLE event_tag ADD CONSTRAINT FK_1 FOREIGN KEY (event_id) REFERENCES event (id);
CREATE INDEX IDX_2 ON event (start_date);
UPDATE event SET summary = LEFT(description, 255) WHERE summary IS NULL;

-- Version DoctrineMigrations\Version20261013110000
ALTER TABLE event DROP old_location, DROP INDEX IDX_3, ADD visits INT NOT NULL;
ALTER TABLE user CHANGE name full_name VARCHAR(255) NOT NULL,
    CHANGE `email` `email` VARCHAR(320) NOT NULL;
ALTER TABLE event MODIFY COLUMN title VARCHAR(100) NOT NULL;
DROP TABLE legacy_log;
ALTER TABLE `group` RENAME COLUMN slug TO handle, ADD COLUMN IF NOT EXISTS created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- Migration 0042_orders
ALTER TABLE ONLY orders ALTER COLUMN total TYPE numeric(12,2), ADD COLUMN note text NOT NULL;
TRUNCATE sessions;
DELETE FROM config WHERE name = 'old, unused';
