-- Academic titles were stored with the degree before the academic rank
-- ("ดร. รศ.") which is the wrong Thai convention — the rank must lead
-- ("รศ. ดร."). Correct the existing rows to match the fixed dropdown options.
UPDATE users SET title = 'ผศ. ดร.' WHERE title = 'ดร. ผศ.';
UPDATE users SET title = 'รศ. ดร.' WHERE title = 'ดร. รศ.';
UPDATE users SET title = 'ศ. ดร.'  WHERE title = 'ดร. ศ.';
