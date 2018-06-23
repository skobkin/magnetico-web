<?php

namespace App\Form;

use Symfony\Component\Validator\Constraints as Assert;

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
     * @Assert\Length(min="8")
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
}