-- Unify kind=http action sources into one structured HTTPSource representation.
-- Pre-existing manual http actions stored a bare upstream URL string; rewrite each
-- into the canonical JSON form {type:"http", method:"POST", base_url:<url>, path:""},
-- which reproduces the original always-POST-to-URL behavior. OpenAPI-imported
-- actions already store valid HTTPSource JSON (json_valid=1) and are left untouched.
UPDATE actions
SET source = json_object('type', 'http', 'method', 'POST', 'base_url', source, 'path', '')
WHERE kind = 'http'
  AND source <> ''
  AND json_valid(source) = 0;
