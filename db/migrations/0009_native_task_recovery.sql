-- Sticky native-use tracking and the durable recovery hold are intentionally
-- separate: ordinary native execution marks use, while crash/lease recovery
-- establishes the hold that requires explicit acknowledgement.
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS native_used BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS native_recovery_hold BOOLEAN NOT NULL DEFAULT false;
