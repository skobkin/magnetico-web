<?php

declare(strict_types=1);

namespace DoctrineMigrations;

use Doctrine\DBAL\Schema\Schema;
use Doctrine\Migrations\AbstractMigration;

final class Version20250701154230 extends AbstractMigration
{
    public function getDescription(): string
    {
        return 'Fixes auto-increment for users and invites id columns by setting default sequence and ownership.';
    }

    public function up(Schema $schema): void
    {
        $this->addSql("ALTER TABLE users.users ALTER COLUMN id SET DEFAULT nextval('users.users_id_seq')");
        $this->addSql("ALTER SEQUENCE users.users_id_seq OWNED BY users.users.id");
        $this->addSql("SELECT setval('users.users_id_seq', COALESCE((SELECT MAX(id) FROM users.users), 1), false)");

        $this->addSql("ALTER TABLE users.invites ALTER COLUMN id SET DEFAULT nextval('users.invites_id_seq')");
        $this->addSql("ALTER SEQUENCE users.invites_id_seq OWNED BY users.invites.id");
        $this->addSql("SELECT setval('users.invites_id_seq', COALESCE((SELECT MAX(id) FROM users.invites), 1), false)");
    }

    public function down(Schema $schema): void
    {
        $this->addSql("ALTER TABLE users.users ALTER COLUMN id DROP DEFAULT");
        $this->addSql("ALTER SEQUENCE users.users_id_seq OWNED BY NONE");

        $this->addSql("ALTER TABLE users.invites ALTER COLUMN id DROP DEFAULT");
        $this->addSql("ALTER SEQUENCE users.invites_id_seq OWNED BY NONE");
    }
}
