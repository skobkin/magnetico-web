-- Task 07C / W2 stage S1: pg_dump --schema-only -n magnetico_web of a disposable PostgreSQL 17 database
-- produced by the real binary: migrate schema-init, then migrate web-schema-init (run twice; second run a no-op).
--
-- PostgreSQL database dump
--

\restrict aBeJIMTjfLznGJJKCaoEX8x52erXp1u22PQg7e9GIqa5Nqpcc3nlINnYXMwqPKp

-- Dumped from database version 17.12 (Debian 17.12-1.pgdg13+1)
-- Dumped by pg_dump version 17.12 (Debian 17.12-1.pgdg13+1)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: magnetico_web; Type: SCHEMA; Schema: -; Owner: postgres
--

CREATE SCHEMA magnetico_web;


ALTER SCHEMA magnetico_web OWNER TO postgres;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: api_tokens; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.api_tokens (
    id bigint NOT NULL,
    token_hash bytea NOT NULL,
    user_id bigint NOT NULL,
    label text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    CONSTRAINT api_tokens_label_length CHECK (((label IS NULL) OR ((char_length(label) >= 1) AND (char_length(label) <= 100)))),
    CONSTRAINT api_tokens_token_hash_len CHECK ((octet_length(token_hash) = 32))
);


ALTER TABLE magnetico_web.api_tokens OWNER TO postgres;

--
-- Name: api_tokens_id_seq; Type: SEQUENCE; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE magnetico_web.api_tokens ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME magnetico_web.api_tokens_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: invites; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.invites (
    id bigint NOT NULL,
    owner_id bigint NOT NULL,
    code text NOT NULL,
    used_by bigint,
    used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    CONSTRAINT invites_code_format CHECK ((code ~ '^[A-Za-z0-9_-]{22,64}$'::text)),
    CONSTRAINT invites_used_at_needs_used_by CHECK (((used_at IS NULL) OR (used_by IS NOT NULL)))
);


ALTER TABLE magnetico_web.invites OWNER TO postgres;

--
-- Name: invites_id_seq; Type: SEQUENCE; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE magnetico_web.invites ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME magnetico_web.invites_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: one_time_tokens; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.one_time_tokens (
    token_hash bytea NOT NULL,
    user_id bigint NOT NULL,
    purpose text NOT NULL,
    email text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT one_time_tokens_email_iff_verify CHECK (((purpose = 'email_verify'::text) = (email IS NOT NULL))),
    CONSTRAINT one_time_tokens_email_length CHECK (((email IS NULL) OR ((char_length(email) >= 3) AND (char_length(email) <= 254)))),
    CONSTRAINT one_time_tokens_expiry_after_creation CHECK ((expires_at > created_at)),
    CONSTRAINT one_time_tokens_purpose_check CHECK ((purpose = ANY (ARRAY['setup'::text, 'reset'::text, 'email_verify'::text]))),
    CONSTRAINT one_time_tokens_token_hash_len CHECK ((octet_length(token_hash) = 32))
);


ALTER TABLE magnetico_web.one_time_tokens OWNER TO postgres;

--
-- Name: rss_tokens; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.rss_tokens (
    id bigint NOT NULL,
    token_hash bytea NOT NULL,
    user_id bigint NOT NULL,
    label text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    legacy boolean DEFAULT false NOT NULL,
    CONSTRAINT rss_tokens_label_length CHECK (((label IS NULL) OR ((char_length(label) >= 1) AND (char_length(label) <= 100)))),
    CONSTRAINT rss_tokens_token_hash_len CHECK ((octet_length(token_hash) = 32))
);


ALTER TABLE magnetico_web.rss_tokens OWNER TO postgres;

--
-- Name: rss_tokens_id_seq; Type: SEQUENCE; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE magnetico_web.rss_tokens ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME magnetico_web.rss_tokens_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: sessions; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.sessions (
    token_hash bytea NOT NULL,
    user_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    remember boolean NOT NULL,
    CONSTRAINT sessions_expiry_after_creation CHECK ((expires_at > created_at)),
    CONSTRAINT sessions_token_hash_len CHECK ((octet_length(token_hash) = 32))
);


ALTER TABLE magnetico_web.sessions OWNER TO postgres;

--
-- Name: settings; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.settings (
    key text NOT NULL,
    value jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT settings_key_format CHECK ((key ~ '^[a-z][a-z0-9_.-]{0,99}$'::text))
);


ALTER TABLE magnetico_web.settings OWNER TO postgres;

--
-- Name: trackers; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.trackers (
    id bigint NOT NULL,
    url text NOT NULL,
    "position" integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT trackers_position_positive CHECK (("position" > 0)),
    CONSTRAINT trackers_url_format CHECK (((char_length(url) <= 2048) AND (url ~* '^(udp|https?|wss?)://[^[:space:]]+$'::text)))
);


ALTER TABLE magnetico_web.trackers OWNER TO postgres;

--
-- Name: trackers_id_seq; Type: SEQUENCE; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE magnetico_web.trackers ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME magnetico_web.trackers_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: users; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.users (
    id bigint NOT NULL,
    username text NOT NULL,
    email text,
    email_verified_at timestamp with time zone,
    password_hash text,
    active boolean DEFAULT true NOT NULL,
    role text DEFAULT 'user'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    password_changed_at timestamp with time zone,
    last_login_at timestamp with time zone,
    CONSTRAINT users_email_length CHECK (((email IS NULL) OR ((char_length(email) >= 3) AND (char_length(email) <= 254)))),
    CONSTRAINT users_password_hash_nonempty CHECK (((password_hash IS NULL) OR (password_hash <> ''::text))),
    CONSTRAINT users_role_check CHECK ((role = ANY (ARRAY['user'::text, 'admin'::text]))),
    CONSTRAINT users_username_length CHECK (((char_length(username) >= 1) AND (char_length(username) <= 25))),
    CONSTRAINT users_verified_needs_email CHECK (((email_verified_at IS NULL) OR (email IS NOT NULL)))
);


ALTER TABLE magnetico_web.users OWNER TO postgres;

--
-- Name: users_id_seq; Type: SEQUENCE; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE magnetico_web.users ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME magnetico_web.users_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: web_version; Type: TABLE; Schema: magnetico_web; Owner: postgres
--

CREATE TABLE magnetico_web.web_version (
    version integer NOT NULL,
    applied_on timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE magnetico_web.web_version OWNER TO postgres;

--
-- Name: api_tokens api_tokens_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.api_tokens
    ADD CONSTRAINT api_tokens_pkey PRIMARY KEY (id);


--
-- Name: api_tokens api_tokens_token_hash_key; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.api_tokens
    ADD CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash);


--
-- Name: invites invites_code_key; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.invites
    ADD CONSTRAINT invites_code_key UNIQUE (code);


--
-- Name: invites invites_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.invites
    ADD CONSTRAINT invites_pkey PRIMARY KEY (id);


--
-- Name: one_time_tokens one_time_tokens_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.one_time_tokens
    ADD CONSTRAINT one_time_tokens_pkey PRIMARY KEY (token_hash);


--
-- Name: rss_tokens rss_tokens_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.rss_tokens
    ADD CONSTRAINT rss_tokens_pkey PRIMARY KEY (id);


--
-- Name: rss_tokens rss_tokens_token_hash_key; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.rss_tokens
    ADD CONSTRAINT rss_tokens_token_hash_key UNIQUE (token_hash);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (token_hash);


--
-- Name: settings settings_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.settings
    ADD CONSTRAINT settings_pkey PRIMARY KEY (key);


--
-- Name: trackers trackers_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.trackers
    ADD CONSTRAINT trackers_pkey PRIMARY KEY (id);


--
-- Name: trackers trackers_url_key; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.trackers
    ADD CONSTRAINT trackers_url_key UNIQUE (url);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: users users_username_key; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.users
    ADD CONSTRAINT users_username_key UNIQUE (username);


--
-- Name: web_version web_version_pkey; Type: CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.web_version
    ADD CONSTRAINT web_version_pkey PRIMARY KEY (version);


--
-- Name: api_tokens_user_id_idx; Type: INDEX; Schema: magnetico_web; Owner: postgres
--

CREATE INDEX api_tokens_user_id_idx ON magnetico_web.api_tokens USING btree (user_id);


--
-- Name: invites_owner_id_idx; Type: INDEX; Schema: magnetico_web; Owner: postgres
--

CREATE INDEX invites_owner_id_idx ON magnetico_web.invites USING btree (owner_id);


--
-- Name: one_time_tokens_one_per_kind; Type: INDEX; Schema: magnetico_web; Owner: postgres
--

CREATE UNIQUE INDEX one_time_tokens_one_per_kind ON magnetico_web.one_time_tokens USING btree (user_id, ((purpose = 'email_verify'::text)));


--
-- Name: rss_tokens_user_id_idx; Type: INDEX; Schema: magnetico_web; Owner: postgres
--

CREATE INDEX rss_tokens_user_id_idx ON magnetico_web.rss_tokens USING btree (user_id);


--
-- Name: sessions_user_id_idx; Type: INDEX; Schema: magnetico_web; Owner: postgres
--

CREATE INDEX sessions_user_id_idx ON magnetico_web.sessions USING btree (user_id);


--
-- Name: users_verified_email_key; Type: INDEX; Schema: magnetico_web; Owner: postgres
--

CREATE UNIQUE INDEX users_verified_email_key ON magnetico_web.users USING btree (lower(email)) WHERE (email_verified_at IS NOT NULL);


--
-- Name: api_tokens api_tokens_user_fkey; Type: FK CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.api_tokens
    ADD CONSTRAINT api_tokens_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id);


--
-- Name: invites invites_owner_fkey; Type: FK CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.invites
    ADD CONSTRAINT invites_owner_fkey FOREIGN KEY (owner_id) REFERENCES magnetico_web.users(id);


--
-- Name: invites invites_used_by_fkey; Type: FK CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.invites
    ADD CONSTRAINT invites_used_by_fkey FOREIGN KEY (used_by) REFERENCES magnetico_web.users(id);


--
-- Name: one_time_tokens one_time_tokens_user_fkey; Type: FK CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.one_time_tokens
    ADD CONSTRAINT one_time_tokens_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id);


--
-- Name: rss_tokens rss_tokens_user_fkey; Type: FK CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.rss_tokens
    ADD CONSTRAINT rss_tokens_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id);


--
-- Name: sessions sessions_user_fkey; Type: FK CONSTRAINT; Schema: magnetico_web; Owner: postgres
--

ALTER TABLE ONLY magnetico_web.sessions
    ADD CONSTRAINT sessions_user_fkey FOREIGN KEY (user_id) REFERENCES magnetico_web.users(id);


--
-- PostgreSQL database dump complete
--

\unrestrict aBeJIMTjfLznGJJKCaoEX8x52erXp1u22PQg7e9GIqa5Nqpcc3nlINnYXMwqPKp

