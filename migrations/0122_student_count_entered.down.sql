ALTER TABLE teaching_courses
    DROP COLUMN IF EXISTS num_students_regular_entered,
    DROP COLUMN IF EXISTS num_students_special_entered;
