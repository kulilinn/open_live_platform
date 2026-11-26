ALTER TABLE message_config ADD COLUMN IF NOT EXISTS welcome TEXT NOT NULL DEFAULT '';

CREATE TABLE static_review_versions (
    kind SMALLINT NOT NULL,
    owner_id BIGINT NOT NULL,
    object_id BIGINT NOT NULL,
    slot INTEGER NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY(kind,owner_id,object_id,slot)
);

CREATE TABLE static_review_requests (
    request_id TEXT PRIMARY KEY,
    kind SMALLINT NOT NULL,
    owner_id BIGINT NOT NULL REFERENCES users(id),
    object_id BIGINT NOT NULL,
    callback_object BIGINT NOT NULL,
    slot INTEGER NOT NULL,
    content TEXT NOT NULL,
    version BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    operation INTEGER,
    applied BOOLEAN NOT NULL DEFAULT FALSE,
    decided_at TIMESTAMPTZ
);

CREATE FUNCTION bump_static_review(k INTEGER, owner BIGINT, subject BIGINT, field_slot INTEGER, old_content TEXT, new_content TEXT)
RETURNS void LANGUAGE sql AS $$
    INSERT INTO static_review_versions(kind,owner_id,object_id,slot)
    SELECT k,owner,subject,field_slot WHERE COALESCE(old_content,'') IS DISTINCT FROM COALESCE(new_content,'')
    ON CONFLICT(kind,owner_id,object_id,slot) DO UPDATE SET version=static_review_versions.version+1;
$$;

CREATE FUNCTION track_static_content() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    previous jsonb := '{}';
    current jsonb := to_jsonb(NEW);
    owner bigint;
    subject bigint;
    n integer;
BEGIN
    IF TG_OP='UPDATE' THEN previous:=to_jsonb(OLD); END IF;
    CASE TG_TABLE_NAME
        WHEN 'users' THEN
            owner:=(current->>'id')::bigint;
            PERFORM bump_static_review(7,owner,owner,0,previous#>>'{profile,Banner}',current#>>'{profile,Banner}');
            PERFORM bump_static_review(9,owner,owner,0,previous#>>'{profile,Photo}',current#>>'{profile,Photo}');
            PERFORM bump_static_review(10,owner,owner,0,previous->>'account',current->>'account');
            PERFORM bump_static_review(11,owner,owner,0,COALESCE(previous#>>'{profile,SearchName}',previous->>'account'),
                COALESCE(current#>>'{profile,SearchName}',current->>'account'));
        WHEN 'anchor_profiles' THEN
            owner:=(current->>'user_id')::bigint;
            PERFORM bump_static_review(8,owner,owner,0,previous#>>'{details,Intro}',current#>>'{details,Intro}');
        WHEN 'message_config' THEN
            owner:=(current->>'user_id')::bigint;
            PERFORM bump_static_review(3,owner,owner,0,previous->>'welcome',current->>'welcome');
        WHEN 'rooms' THEN
            owner:=(current->>'owner_id')::bigint;
            subject:=(current->>'id')::bigint;
            PERFORM bump_static_review(2,owner,subject,0,previous->>'offline_info',current->>'offline_info');
            PERFORM bump_static_review(4,owner,subject,0,previous->>'title',current->>'title');
            PERFORM bump_static_review(5,owner,subject,0,previous->>'banner',current->>'banner');
        WHEN 'subscription_plans' THEN
            owner:=(current->>'anchor_id')::bigint;
            subject:=(current->>'id')::bigint;
            PERFORM bump_static_review(1,owner,subject,0,previous->>'name',current->>'name');
        WHEN 'room_wheel_configs' THEN
            owner:=(current->>'anchor_id')::bigint;
            FOR n IN 0..greatest(jsonb_array_length(previous->'options'),jsonb_array_length(current->'options'))-1 LOOP
                PERFORM bump_static_review(6,owner,owner,n+1,previous->'options'->>n,current->'options'->>n);
            END LOOP;
    END CASE;
    RETURN NULL;
END;
$$;

CREATE TRIGGER users_static_review AFTER INSERT OR UPDATE OF account,profile ON users
    FOR EACH ROW EXECUTE FUNCTION track_static_content();
CREATE TRIGGER profiles_static_review AFTER INSERT OR UPDATE OF details ON anchor_profiles
    FOR EACH ROW EXECUTE FUNCTION track_static_content();
CREATE TRIGGER welcome_static_review AFTER INSERT OR UPDATE OF welcome ON message_config
    FOR EACH ROW EXECUTE FUNCTION track_static_content();
CREATE TRIGGER room_static_review AFTER INSERT OR UPDATE OF title,banner,offline_info ON rooms
    FOR EACH ROW EXECUTE FUNCTION track_static_content();
CREATE TRIGGER plan_static_review AFTER INSERT OR UPDATE OF name ON subscription_plans
    FOR EACH ROW EXECUTE FUNCTION track_static_content();
CREATE TRIGGER wheel_static_review AFTER INSERT OR UPDATE OF options ON room_wheel_configs
    FOR EACH ROW EXECUTE FUNCTION track_static_content();
