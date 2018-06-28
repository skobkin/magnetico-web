<?php

namespace App\FormRequest;

use Symfony\Component\Validator\Constraints as Assert;

/**
 * @todo implement UniqueEntity constraint for DTO and use it here
 */
class CreateUserRequest
{
    /**
     * @var string
     *
     * @Assert\NotBlank()
     * @Assert\Length(min="2", max="25")
     */
    public $username;

    /**
     * @var string
     *
     * @Assert\NotBlank()
     * @Assert\Length(min="8", max="4096")
     */
    public $password;

    /**
     * @var string
     *
     * @Assert\Email()
     */
    public $email;

    /**
     * @var string
     *
     * @Assert\NotBlank()
     * @Assert\Length(min="32", max="32")
     */
    public $inviteCode;

    public function __construct(string $inviteCode = null)
    {
        $this->inviteCode = $inviteCode;
    }
}