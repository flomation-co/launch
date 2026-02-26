CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TYPE TriggerType AS ENUM('manual', 'scheduled', 'qr', 'image', 'email', 'telegram', 'form');

CREATE TABLE trigger (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    type TriggerType NOT NULL DEFAULT 'manual',
    data JSONB NOT NULL DEFAULT '{}',
    flow_id UUID NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    disabled_at TIMESTAMP
);