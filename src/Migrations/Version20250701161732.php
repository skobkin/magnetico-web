<?php

declare(strict_types=1);

namespace DoctrineMigrations;

use Doctrine\DBAL\Schema\Schema;
use Doctrine\Migrations\AbstractMigration;

final class Version20250701161732 extends AbstractMigration
{
    public function getDescription(): string
    {
        return 'Fix identity sequence values for users and invites tables to start from MAX(id) + 1.';
    }

    public function up(Schema $schema): void
    {
        $this->addSql("DO \$do\$ DECLARE max_id integer; BEGIN SELECT COALESCE(MAX(id), 0) + 1 INTO max_id FROM users.users; EXECUTE format('ALTER TABLE users.users ALTER COLUMN id RESTART WITH %s', max_id); END \$do\$;");
        
        $this->addSql("DO \$do\$ DECLARE max_id integer; BEGIN SELECT COALESCE(MAX(id), 0) + 1 INTO max_id FROM users.invites; EXECUTE format('ALTER TABLE users.invites ALTER COLUMN id RESTART WITH %s', max_id); END \$do\$;");
    }

    public function down(Schema $schema): void
    {
        $this->addSql("ALTER TABLE users.users ALTER COLUMN id RESTART WITH 1");
        $this->addSql("ALTER TABLE users.invites ALTER COLUMN id RESTART WITH 1");
    }
}
