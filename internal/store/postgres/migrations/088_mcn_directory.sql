CREATE SEQUENCE mcn_directory_id_seq AS BIGINT START 6755399441055744;

CREATE TABLE mcn_agency_directory_changes (
    id bigint PRIMARY KEY DEFAULT nextval('mcn_directory_id_seq') CHECK(id>0),
    sharing_template_id bigint,
    name text NOT NULL,
    super_name text NOT NULL,
    email text NOT NULL,
    agency_level integer NOT NULL,
    ratio numeric NOT NULL DEFAULT 0,
    disabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT statement_timestamp()
);

CREATE VIEW mcn_agency_directory AS
SELECT id,sharing_template_id,name,super_name,email,agency_level,ratio,disabled,created_at FROM mcn_agency_directory_changes
UNION ALL
SELECT h.source_id::bigint,j.sharing_template_id,j.name,COALESCE(j.super_name,''),COALESCE(j.email,''),COALESCE(j.agency_level,0),
    COALESCE(j.ratio,0),COALESCE(j.status,0)<>0,j.create_time AT TIME ZONE 'UTC'
FROM legacy_business_history h CROSS JOIN LATERAL jsonb_to_record(h.record) j
    (sharing_template_id bigint,name text,super_name text,email text,agency_level int,ratio numeric,status smallint,create_time timestamp)
WHERE h.source_table='mcn_info' AND NOT EXISTS(SELECT 1 FROM mcn_agency_directory_changes c WHERE c.id=h.source_id::bigint);

CREATE TABLE mcn_template_changes (
    id bigint PRIMARY KEY DEFAULT nextval('mcn_directory_id_seq') CHECK(id>0),
    template_id bigint NOT NULL CHECK(template_id>0),
    agency_id bigint NOT NULL CHECK(agency_id>=0),
    name text NOT NULL,
    lower_bound bigint NOT NULL CHECK(lower_bound>=0),
    upper_bound bigint NOT NULL CHECK(upper_bound>=lower_bound),
    ratio numeric NOT NULL CHECK(ratio BETWEEN 0 AND 100),
    disabled boolean NOT NULL DEFAULT false
);
CREATE INDEX mcn_template_changes_group ON mcn_template_changes(template_id,agency_id);

CREATE VIEW mcn_template_directory AS
SELECT id,template_id,agency_id,name,lower_bound,upper_bound,ratio,disabled FROM mcn_template_changes
UNION ALL
SELECT h.source_id::bigint,j.template_id,h.owner_id,j.name,j.min,j.max,j.ratio,COALESCE(j.status,0)<>0
FROM legacy_business_history h CROSS JOIN LATERAL jsonb_to_record(h.record) j
    (template_id bigint,name text,min bigint,max bigint,ratio numeric,status smallint)
WHERE h.source_table='mcn_sharing_template' AND NOT EXISTS(SELECT 1 FROM mcn_template_changes c WHERE c.id=h.source_id::bigint);
