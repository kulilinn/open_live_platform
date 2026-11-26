CREATE SEQUENCE backoffice_system_id_seq AS BIGINT START WITH 4503599627370496;

CREATE TABLE backoffice_system_tenants (
    code TEXT PRIMARY KEY,
    id BIGINT NOT NULL UNIQUE DEFAULT nextval('backoffice_system_id_seq'),
    name TEXT NOT NULL,
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);

CREATE TABLE backoffice_system_accounts (
    area TEXT NOT NULL CHECK (area IN ('admin','mcn')),
    scope TEXT NOT NULL CHECK (scope <> ''),
    id BIGINT NOT NULL DEFAULT nextval('backoffice_system_id_seq') CHECK (id > 0),
    username TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL,
    email TEXT NOT NULL DEFAULT '',
    phone TEXT NOT NULL DEFAULT '',
    gender SMALLINT NOT NULL DEFAULT 0,
    account_type SMALLINT NOT NULL DEFAULT 0,
    agency_level INTEGER NOT NULL DEFAULT 0,
    administrator BOOLEAN NOT NULL DEFAULT FALSE,
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    deleted BOOLEAN NOT NULL DEFAULT FALSE,
    remark TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    credential_version UUID NOT NULL DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    CHECK ((area='admin' AND username <> '') OR (area='mcn' AND email <> '' AND scope ~ '^[1-9][0-9]*$')),
    PRIMARY KEY (area,id),
    UNIQUE (area,scope,id)
);
CREATE UNIQUE INDEX backoffice_system_username_idx ON backoffice_system_accounts(scope,lower(username)) WHERE area='admin';
CREATE UNIQUE INDEX backoffice_system_mcn_email_idx ON backoffice_system_accounts(lower(email)) WHERE area='mcn';

CREATE TABLE backoffice_system_bootstrap (
    area TEXT NOT NULL CHECK (area IN ('admin','mcn')),
    id BIGINT NOT NULL CHECK (id > 0),
    scope TEXT NOT NULL,
    login TEXT NOT NULL,
    PRIMARY KEY (area,id)
);
CREATE UNIQUE INDEX backoffice_bootstrap_username_idx ON backoffice_system_bootstrap(scope,lower(login)) WHERE area='admin';
CREATE UNIQUE INDEX backoffice_bootstrap_mcn_email_idx ON backoffice_system_bootstrap(lower(login)) WHERE area='mcn';

CREATE TABLE backoffice_system_roles (
    area TEXT NOT NULL CHECK (area IN ('admin','mcn')),
    scope TEXT NOT NULL CHECK (scope <> ''),
    id BIGINT NOT NULL DEFAULT nextval('backoffice_system_id_seq') CHECK (id > 0),
    name TEXT NOT NULL,
    code TEXT NOT NULL DEFAULT '',
    sort INTEGER NOT NULL DEFAULT 0,
    status SMALLINT NOT NULL DEFAULT 0,
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (area,scope,id)
);

CREATE TABLE backoffice_system_menus (
    area TEXT NOT NULL CHECK (area IN ('admin','mcn')),
    scope TEXT NOT NULL CHECK (scope <> ''),
    id BIGINT NOT NULL DEFAULT nextval('backoffice_system_id_seq') CHECK (id > 0),
    parent_id BIGINT,
    name TEXT NOT NULL,
    url TEXT NOT NULL DEFAULT '',
    menu_type SMALLINT NOT NULL DEFAULT 0,
    hidden SMALLINT NOT NULL DEFAULT 0,
    target SMALLINT NOT NULL DEFAULT 1,
    refresh SMALLINT NOT NULL DEFAULT 0,
    permission TEXT NOT NULL DEFAULT '',
    icon TEXT NOT NULL DEFAULT '',
    sort INTEGER NOT NULL DEFAULT 0,
    remark TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (area,scope,id),
    FOREIGN KEY (area,scope,parent_id) REFERENCES backoffice_system_menus(area,scope,id) DEFERRABLE,
    CHECK (parent_id IS DISTINCT FROM id)
);

CREATE TABLE backoffice_system_members (
    area TEXT NOT NULL,
    scope TEXT NOT NULL,
    account_id BIGINT NOT NULL,
    role_id BIGINT NOT NULL,
    group_admin BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (area,scope,account_id,role_id),
    FOREIGN KEY (area,scope,account_id) REFERENCES backoffice_system_accounts(area,scope,id) ON DELETE CASCADE,
    FOREIGN KEY (area,scope,role_id) REFERENCES backoffice_system_roles(area,scope,id) ON DELETE CASCADE
);
CREATE INDEX backoffice_system_members_role_idx ON backoffice_system_members(area,scope,role_id);

CREATE TABLE backoffice_system_role_menus (
    area TEXT NOT NULL,
    scope TEXT NOT NULL,
    role_id BIGINT NOT NULL,
    menu_id BIGINT NOT NULL,
    PRIMARY KEY (area,scope,role_id,menu_id),
    FOREIGN KEY (area,scope,role_id) REFERENCES backoffice_system_roles(area,scope,id) ON DELETE CASCADE,
    FOREIGN KEY (area,scope,menu_id) REFERENCES backoffice_system_menus(area,scope,id) ON DELETE CASCADE
);
CREATE INDEX backoffice_system_role_menus_menu_idx ON backoffice_system_role_menus(area,scope,menu_id);

CREATE TABLE backoffice_system_user_menus (
    area TEXT NOT NULL CHECK (area='mcn'),
    scope TEXT NOT NULL,
    account_id BIGINT NOT NULL,
    menu_id BIGINT NOT NULL,
    PRIMARY KEY (area,scope,account_id,menu_id),
    FOREIGN KEY (area,scope,account_id) REFERENCES backoffice_system_accounts(area,scope,id) ON DELETE CASCADE,
    FOREIGN KEY (area,scope,menu_id) REFERENCES backoffice_system_menus(area,scope,id) ON DELETE CASCADE
);
CREATE INDEX backoffice_system_user_menus_menu_idx ON backoffice_system_user_menus(area,scope,menu_id);
