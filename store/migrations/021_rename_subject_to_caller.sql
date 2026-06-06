ALTER TABLE transactions RENAME COLUMN subject_user_id TO caller_user_id;
ALTER TABLE acl_entries RENAME COLUMN subject_user_id TO caller_user_id;
ALTER TABLE process_authorities RENAME COLUMN subject_user_id TO caller_user_id;
