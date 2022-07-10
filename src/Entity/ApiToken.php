<?php
declare(strict_types=1);

namespace App\Entity;

use App\Repository\ApiTokenRepository;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Component\Serializer\Annotation as Serializer;

#[ORM\Table(name: 'api_tokens', schema: 'users')]
#[ORM\Entity(repositoryClass: ApiTokenRepository::class, readOnly: true)]
class ApiToken
{
    #[ORM\ManyToOne(targetEntity: User::class)]
    #[ORM\JoinColumn(name: 'user_id', nullable: false)]
    private User $user;

    #[Serializer\Groups(['api', 'api_v1_login'])]
    #[ORM\Id]
    #[ORM\Column(name: 'key', type: 'string', length: 32)]
    private string $key;

    #[ORM\Column(name: 'created_at', type: 'datetime')]
    private \DateTime $createdAt;

    public function __construct(User $user)
    {
        $this->user = $user;
        $this->key = md5(random_bytes(100));
        $this->createdAt = new \DateTime();
    }

    public function getUser(): User
    {
        return $this->user;
    }

    public function getKey(): string
    {
        return $this->key;
    }

    public function getCreatedAt(): \DateTime
    {
        return $this->createdAt;
    }
}
