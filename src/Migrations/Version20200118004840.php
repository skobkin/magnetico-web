<?php

declare(strict_types=1);

namespace DoctrineMigrations;

use Doctrine\DBAL\Schema\Schema;
use Doctrine\Migrations\AbstractMigration;

final class Version20200118004840 extends AbstractMigration
{
    public function getDescription() : string
    {
        return 'Adding password reset tokens.';
    }

    public function up(Schema $schema) : void
    {
        // this up() migration is auto-generated, please modify it to your needs
        $this->abortIf($this->connection->getDatabasePlatform()->getName() !== 'postgresql', 'Migration can only be executed safely on \'postgresql\'.');

        $this->addSql('CREATE TABLE users.password_reset_tokens (code TEXT NOT NULL, user_id INT NOT NULL, valid_until TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, PRIMARY KEY(code))');
        $this->addSql('CREATE INDEX IDX_261A8E65A76ED395 ON users.password_reset_tokens (user_id)');
        $this->addSql('ALTER TABLE users.password_reset_tokens ADD CONSTRAINT FK_CCD4B965A76ED395 FOREIGN KEY (user_id) REFERENCES users.users (id) ON DELETE CASCADE NOT DEFERRABLE INITIALLY IMMEDIATE');
    }

    public function down(Schema $schema) : void
    {
        // this down() migration is auto-generated, please modify it to your needs
        $this->abortIf($this->connection->getDatabasePlatform()->getName() !== 'postgresql', 'Migration can only be executed safely on \'postgresql\'.');

        $this->addSql('DROP TABLE users.password_reset_tokens');
    }
}
