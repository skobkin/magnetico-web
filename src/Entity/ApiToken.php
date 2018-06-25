<?php

namespace App\Entity;

use Doctrine\ORM\Mapping as ORM;

/**
 * @ORM\Table(name="api_tokens", schema="users")
 * @ORM\Entity(repositoryClass="App\Repository\ApiTokenRepository", readOnly=true)
 */
class ApiToken
{
    /**
     * @var User
     *
     * @ORM\ManyToOne(targetEntity="App\Entity\User")
     * @ORM\JoinColumn(name="user_id", nullable=false)
     */
    private $user;

    /**
     * @var string
     *
     * @ORM\Id()
     * @ORM\Column(name="key", type="string", length=32)
     */
    private $key;

    /**
     * @var \DateTime
     *
     * @ORM\Column(name="created_at", type="datetime")
     */
    private $createdAt;

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