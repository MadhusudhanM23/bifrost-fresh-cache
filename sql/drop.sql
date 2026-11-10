-- Removes everything cache_insights created. Does not touch the cache table.
SET client_min_messages = warning;
DROP SCHEMA IF EXISTS cache_insights CASCADE;
