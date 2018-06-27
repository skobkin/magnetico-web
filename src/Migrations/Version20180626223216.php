<?php declare(strict_types=1);

namespace DoctrineMigrations;

use Doctrine\DBAL\Schema\Schema;
use Doctrine\Migrations\AbstractMigration;

/**
 * Creates users schema with basic set of tables.
 */
final class Version20180626223216 extends AbstractMigration
{
    public function up(Schema $schema) : void
    {
        // this up() migration is auto-generated, please modify it to your needs
        $this->abortIf($this->connection->getDatabasePlatform()->getName() !== 'postgresql', 'Migration can only be executed safely on \'postgresql\'.');

        $this->addSql('CREATE SCHEMA users');
        $this->addSql('CREATE SEQUENCE users.users_id_seq INCREMENT BY 1 MINVALUE 1 START 1');
        $this->addSql('CREATE SEQUENCE users.invites_id_seq INCREMENT BY 1 MINVALUE 1 START 1');
        $this->addSql('CREATE TABLE users.users (id INT NOT NULL, username VARCHAR(25) NOT NULL, password TEXT NOT NULL, email VARCHAR(254) NOT NULL, roles JSON NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, PRIMARY KEY(id))');
        $this->addSql('CREATE UNIQUE INDEX UNIQ_338ADFC4F85E0677 ON users.users (username)');
        $this->addSql('CREATE UNIQUE INDEX UNIQ_338ADFC4E7927C74 ON users.users (email)');
        $this->addSql('CREATE TABLE users.invites (id INT NOT NULL, user_id INT NOT NULL, used_by_id INT DEFAULT NULL, code VARCHAR(32) NOT NULL, PRIMARY KEY(id))');
        $this->addSql('CREATE UNIQUE INDEX UNIQ_93848FA877153098 ON users.invites (code)');
        $this->addSql('CREATE INDEX IDX_93848FA8A76ED395 ON users.invites (user_id)');
        $this->addSql('CREATE INDEX IDX_93848FA84C2B72A8 ON users.invites (used_by_id)');
        $this->addSql('CREATE TABLE users.api_tokens (key VARCHAR(32) NOT NULL, user_id INT NOT NULL, created_at TIMESTAMP(0) WITHOUT TIME ZONE NOT NULL, PRIMARY KEY(key))');
        $this->addSql('CREATE INDEX IDX_C46A74A9A76ED395 ON users.api_tokens (user_id)');
        $this->addSql('ALTER TABLE users.invites ADD CONSTRAINT FK_93848FA8A76ED395 FOREIGN KEY (user_id) REFERENCES users.users (id) NOT DEFERRABLE INITIALLY IMMEDIATE');
        $this->addSql('ALTER TABLE users.invites ADD CONSTRAINT FK_93848FA84C2B72A8 FOREIGN KEY (used_by_id) REFERENCES users.users (id) NOT DEFERRABLE INITIALLY IMMEDIATE');
        $this->addSql('ALTER TABLE users.api_tokens ADD CONSTRAINT FK_C46A74A9A76ED395 FOREIGN KEY (user_id) REFERENCES users.users (id) NOT DEFERRABLE INITIALLY IMMEDIATE');
    }

    public function down(Schema $schema) : void
    {
        // this down() migration is auto-generated, please modify it to your needs
        $this->abortIf($this->connection->getDatabasePlatform()->getName() !== 'postgresql', 'Migration can only be executed safely on \'postgresql\'.');

        $this->addSql('ALTER TABLE users.invites DROP CONSTRAINT FK_93848FA8A76ED395');
        $this->addSql('ALTER TABLE users.invites DROP CONSTRAINT FK_93848FA84C2B72A8');
        $this->addSql('ALTER TABLE users.api_tokens DROP CONSTRAINT FK_C46A74A9A76ED395');
        $this->addSql('DROP SEQUENCE users.users_id_seq CASCADE');
        $this->addSql('DROP SEQUENCE users.invites_id_seq CASCADE');
        $this->addSql('DROP TABLE users.users');
        $this->addSql('DROP TABLE users.invites');
        $this->addSql('DROP TABLE users.api_tokens');
        $this->addSql('DROP SCHEMA users');
    }
}
