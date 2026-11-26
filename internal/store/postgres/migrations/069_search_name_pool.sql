CREATE TABLE unused_search_names (name text PRIMARY KEY);
CREATE FUNCTION choose_pooled_search_name() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE alias text;
BEGIN
    IF NEW.profile IS NOT NULL THEN RETURN NEW; END IF;
    DELETE FROM unused_search_names WHERE name = (
        SELECT n.name FROM unused_search_names n
        WHERE NOT EXISTS (SELECT 1 FROM users u WHERE lower(COALESCE(u.profile->>'SearchName',u.account))=n.name)
        ORDER BY n.name LIMIT 1 FOR UPDATE SKIP LOCKED
    ) RETURNING name INTO alias;
    IF alias IS NOT NULL THEN
        NEW.profile := jsonb_build_object('Name',NEW.account,'SearchName',alias,'UserType','normal','ToyInfo','{}');
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER assign_pooled_search_name BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION choose_pooled_search_name();
