-- Lets the service roles log in, with the passwords from .env. The roles and their grants come from
-- the migrations; passwords stay out of them because the repo is public. Run by the db-roles
-- Compose service after migrate, and safe to run again.
\getenv gateway_password GATEWAY_DB_PASSWORD
\getenv core_password CORE_DB_PASSWORD
SET log_statement = 'none';
ALTER ROLE kaya_gateway LOGIN PASSWORD :'gateway_password';
ALTER ROLE kaya_core LOGIN PASSWORD :'core_password';
